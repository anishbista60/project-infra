/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2022 Red Hat, Inc.
 *
 */

package cmd

import (
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"fmt"
	"kubevirt.io/project-infra/robots/pkg/flakefinder/build"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/bndr/gojenkins"
	"github.com/joshdk/go-junit"
	log "github.com/sirupsen/logrus"
	"kubevirt.io/project-infra/robots/pkg/flakefinder"
	junitMerge "kubevirt.io/project-infra/robots/pkg/flakefinder/junit-merge"
	flakejenkins "kubevirt.io/project-infra/robots/pkg/jenkins"
)

const (
	defaultJenkinsBaseUrl        = "https://main-jenkins-csb-cnvqe.apps.ocp-c1.prod.psi.redhat.com/"
	defaultJenkinsJobNamePattern = "^test-kubevirt-cnv-4.10-(compute|network|operator|storage)(-[a-z0-9]+)?$"
	defaultArtifactFileNameRegex = "^(xunit_results|((merged|partial)\\.)?junit\\.functest(\\.[0-9]+)?)\\.xml$"
	jenkinsShortUsage            = "flake-report-creator jenkins creates reports from junit xml artifact files generated during jenkins builds"
)

//go:embed jenkins-report-template.gohtml
var jenkinsReportTemplate string

var (
	fileNameRegex  = regexp.MustCompile(defaultArtifactFileNameRegex)
	jobNameRegexes []*regexp.Regexp
	opts           jenkinsOptions
	jenkinsCommand = &cobra.Command{
		Use:   "jenkins",
		Short: jenkinsShortUsage,
		Long: jenkinsShortUsage + `

Examples:

# create a report over all kubevirt testing jenkins jobs matching the default job name pattern for the last 14 days
$ flake-report-creator jenkins

# create a report as above for the last 3 days
$ flake-report-creator jenkins --startFrom=72h

# create a report over kubevirt testing jobs for compute and storage ocs for the given job name pattern for the last 24 hours
$ flake-report-creator jenkins --startFrom=24h --jobNamePattern='^test-kubevirt-cnv-4\.10-(compute|storage)-ocs$'
`,
		RunE: runJenkinsReport,
	}
	jLog = log.StandardLogger().WithField("type", "jenkins")
)

func init() {
	log.SetFormatter(&log.JSONFormatter{})
	opts = jenkinsOptions{}
	jenkinsCommand.PersistentFlags().StringVar(&opts.endpoint, "endpoint", defaultJenkinsBaseUrl, "jenkins base url")
	jenkinsCommand.PersistentFlags().StringArrayVar(&opts.jobNamePatterns, "jobNamePattern", []string{defaultJenkinsJobNamePattern}, "jenkins job name go regex pattern to filter jobs for the report. May appear multiple times.")
	jenkinsCommand.PersistentFlags().StringVar(&opts.artifactFileNameRegex, "artifactFileNamePattern", defaultArtifactFileNameRegex, "artifact file name go regex pattern to fetch junit artifact files")
	jenkinsCommand.PersistentFlags().DurationVar(&opts.startFrom, "startFrom", 14*24*time.Hour, "The duration when the report data should be fetched")
	jenkinsCommand.PersistentFlags().DurationVar(&opts.startFromForRatings, "startFromForRatings", 14*24*time.Hour, "The duration when the rating data should be fetched")
	jenkinsCommand.PersistentFlags().IntVar(&opts.maxConnsPerHost, "maxConnsPerHost", 5, "The maximum number of connections to the endpoint (to avoid getting rate limited)")
	jenkinsCommand.PersistentFlags().BoolVar(&opts.insecureSkipVerify, "insecureSkipVerify", false, "Whether the tls verification should be skipped (this is insecure!)")
}

type jenkinsOptions struct {
	endpoint              string
	jobNamePatterns       []string
	startFrom             time.Duration
	maxConnsPerHost       int
	artifactFileNameRegex string
	insecureSkipVerify    bool
	startFromForRatings   time.Duration
}

type JenkinsReportParams struct {
	flakefinder.Params
	JenkinsBaseURL    string
	JobNamesToRatings map[string]build.Rating
}

type JSONParams struct {
	Data map[string]map[string]*flakefinder.Details `json:"data"`
}

func JenkinsCommand() *cobra.Command {
	return jenkinsCommand
}

func runJenkinsReport(cmd *cobra.Command, args []string) error {
	err := cmd.InheritedFlags().Parse(args)
	if err != nil {
		return err
	}

	if err = globalOpts.Validate(); err != nil {
		_, err2 := fmt.Fprint(cmd.OutOrStderr(), cmd.UsageString(), err)
		if err2 != nil {
			return err2
		}
		return fmt.Errorf("invalid arguments provided: %v", err)
	}

	fileNameRegex, err = regexp.Compile(opts.artifactFileNameRegex)
	if err != nil {
		_, err2 := fmt.Fprint(cmd.OutOrStderr(), cmd.UsageString(), err)
		if err2 != nil {
			return err2
		}
		return fmt.Errorf("invalid arguments provided: %v", err)
	}

	for _, jobNamePattern := range opts.jobNamePatterns {
		jobNameRegex, err := regexp.Compile(jobNamePattern)
		if err != nil {
			_, err2 := fmt.Fprint(cmd.OutOrStderr(), cmd.UsageString(), err)
			if err2 != nil {
				return err2
			}
			jLog.Fatalf("failed to fetch jobs: %v", err)
		}
		jobNameRegexes = append(jobNameRegexes, jobNameRegex)
	}

	ctx := context.Background()

	// limit http client connections to avoid 504 errors, looks like we are getting rate limited
	client := &http.Client{
		Transport: &http.Transport{
			MaxConnsPerHost: opts.maxConnsPerHost,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: opts.insecureSkipVerify,
			},
		},
	}

	jLog.Printf("Creating client for %s", opts.endpoint)
	jenkins := gojenkins.CreateJenkins(client, opts.endpoint)
	_, err = jenkins.Init(ctx)
	if err != nil {
		return fmt.Errorf("failed to contact jenkins %s: %v", opts.endpoint, err)
	}

	jLog.Printf("Fetching jobs")
	jobNames, err := jenkins.GetAllJobNames(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch jobs: %v", err)
	}
	jLog.Printf("Fetched %d jobs", len(jobNames))

	startOfReport := time.Now().Add(-1 * opts.startFrom)
	startOfReportForRatings := time.Now().Add(-1 * opts.startFromForRatings)

	junitReportsFromMatchingJobs, ratings := fetchJunitReportsFromMatchingJobs(startOfReport, startOfReportForRatings, jobNames, jenkins, ctx)
	writeReportToFile(startOfReport, time.Now(), junitReportsFromMatchingJobs, globalOpts.outputFile, ratings)

	return nil
}

func fetchJunitReportsFromMatchingJobs(startOfReport time.Time, startOfReportsForRatings time.Time, innerJobs []gojenkins.InnerJob, jenkins *gojenkins.Jenkins, ctx context.Context) ([]*flakefinder.JobResult, []build.Rating) {
	filteredJobs := filterMatchingJobs(innerJobs)
	return fetchJobReports(startOfReport, startOfReportsForRatings, filteredJobs, jenkins, ctx)
}

func filterMatchingJobs(innerJobs []gojenkins.InnerJob) []gojenkins.InnerJob {
	filteredJobs := []gojenkins.InnerJob{}
	for _, jobNameRegex := range jobNameRegexes {
		jLog.Printf("Filtering for jobs matching %s", jobNameRegex)
		for _, innerJob := range innerJobs {
			if !jobNameRegex.MatchString(innerJob.Name) {
				continue
			}
			filteredJobs = append(filteredJobs, innerJob)
		}
	}
	jLog.Printf("%d jobs left after filtering", len(filteredJobs))
	return filteredJobs
}

func fetchJobReports(startOfReport time.Time, startOfReportForRatings time.Time, filteredJobs []gojenkins.InnerJob, jenkins *gojenkins.Jenkins, ctx context.Context) ([]*flakefinder.JobResult, []build.Rating) {
	resultChan := make(chan result)

	go runReportDataFetches(filteredJobs, jenkins, ctx, startOfReport, startOfReportForRatings, resultChan)

	buildRatings := []build.Rating{}
	reports := []*flakefinder.JobResult{}
	for result := range resultChan {
		reports = append(reports, result.jobResults...)
		buildRatings = append(buildRatings, result.buildRating)
	}
	return reports, buildRatings
}

func runReportDataFetches(filteredJobs []gojenkins.InnerJob, jenkins *gojenkins.Jenkins, ctx context.Context, startOfReport time.Time, startOfReportForRatings time.Time, resultChan chan result) {
	var wg sync.WaitGroup
	wg.Add(len(filteredJobs))

	defer close(resultChan)
	for _, filteredJob := range filteredJobs {
		go fetchReportDataForJob(filteredJob, jenkins, ctx, startOfReport, startOfReportForRatings, &wg, resultChan)
	}

	wg.Wait()
}

type result struct {
	jobResults  []*flakefinder.JobResult
	buildRating build.Rating
}

func fetchReportDataForJob(filteredJob gojenkins.InnerJob, jenkins *gojenkins.Jenkins, ctx context.Context, startOfReport time.Time, startOfReportForRatings time.Time, wg *sync.WaitGroup, resultChan chan result) {
	defer wg.Done()

	fLog := jLog.WithField("job", filteredJob.Name)

	fLog.Printf("Fetching job")
	job, err := jenkins.GetJob(ctx, filteredJob.Name)
	if err != nil {
		fLog.Fatalf("failed to fetch job: %v", err)
	}

	buildNumbersToFailures := flakejenkins.GetBuildNumbersToFailuresForJob(startOfReportForRatings, job, ctx, jLog)
	ratingForBuilds := build.NewRating(filteredJob.Name, opts.endpoint, opts.startFrom, buildNumbersToFailures)

	completedBuilds := flakejenkins.FetchCompletedBuildsForJob(startOfReport, job.Raw.LastBuild.Number, job, ctx, fLog, 4)

	filteredBuilds := filterBuildsByRating(completedBuilds, ratingForBuilds, fLog)

	junitFilesFromArtifacts := fetchJunitFilesFromArtifacts(filteredBuilds, fLog)
	reportsPerJob := convertJunitFileDataToReport(junitFilesFromArtifacts, ctx, job, fLog)

	resultChan <- result{
		jobResults:  reportsPerJob,
		buildRating: ratingForBuilds,
	}
}

func filterBuildsByRating(completedBuilds []*gojenkins.Build, ratingForBuilds build.Rating, fLog *log.Entry) []*gojenkins.Build {
	filteredBuilds := []*gojenkins.Build{}
	for _, completedBuild := range completedBuilds {
		number := completedBuild.GetBuildNumber()
		if ratingForBuilds.ShouldFilterBuild(number) {
			fLog.Warnf("Skipping build %d due to %f sigma rating, %d failures", number, ratingForBuilds.GetBuildData(number).Sigma, ratingForBuilds.GetBuildData(number).Failures)
		} else {
			filteredBuilds = append(filteredBuilds, completedBuild)
		}
	}
	return filteredBuilds
}

func fetchJunitFilesFromArtifacts(completedBuilds []*gojenkins.Build, fLog *log.Entry) []gojenkins.Artifact {
	fLog.Printf("Fetch junit files from artifacts for %d completed builds", len(completedBuilds))
	artifacts := []gojenkins.Artifact{}
	for _, completedBuild := range completedBuilds {
		for _, artifact := range completedBuild.GetArtifacts() {
			if !fileNameRegex.MatchString(artifact.FileName) {
				continue
			}
			artifacts = append(artifacts, artifact)
		}
	}
	fLog.Printf("Fetched %d junit files from artifacts", len(artifacts))
	return artifacts
}

func convertJunitFileDataToReport(junitFilesFromArtifacts []gojenkins.Artifact, ctx context.Context, job *gojenkins.Job, fLog *log.Entry) []*flakefinder.JobResult {

	// problem: we might encounter multiple junit artifacts per job run, we need to merge them into
	// 			one so the report builder can handle the results

	// step 1: download the report junit data and store them in a slice per build
	fLog.Printf("Download %d artifacts and convert to reports", len(junitFilesFromArtifacts))
	artifactsPerBuild := map[int64][][]junit.Suite{}
	for _, artifact := range junitFilesFromArtifacts {
		data, err := artifact.GetData(ctx)
		if err != nil {
			fLog.Fatalf("failed to fetch artifact data: %v", err)
		}
		report, err := junit.Ingest(data)
		if err != nil {
			fLog.Fatalf("failed to fetch artifact data: %v", err)
		}
		buildNumber := artifact.Build.Info().Number
		artifactsPerBuild[buildNumber] = append(artifactsPerBuild[buildNumber], report)
	}

	// step 2: merge all the suites for a build into one suite per build
	fLog.Printf("Merge reports for %d builds", len(artifactsPerBuild))
	reportsPerJob := []*flakefinder.JobResult{}
	for buildNumber, artifacts := range artifactsPerBuild {
		// TODO: evaluate conflicts somehow
		mergedResult, _ := junitMerge.Merge(artifacts)
		reportsPerJob = append(reportsPerJob, &flakefinder.JobResult{Job: job.GetName(), JUnit: mergedResult, BuildNumber: int(buildNumber)})
	}

	return reportsPerJob
}

func writeReportToFile(startOfReport time.Time, endOfReport time.Time, reports []*flakefinder.JobResult, outputFile string, ratings []build.Rating) {
	parameters := flakefinder.CreateFlakeReportData(reports, []int{}, endOfReport, "kubevirt", "kubevirt", startOfReport)
	jLog.Printf("writing output to %s", outputFile)

	jobNamesToRatings := map[string]build.Rating{}
	for _, rating := range ratings {
		jobNamesToRatings[rating.Name] = rating
	}

	writeHTMLReportToOutputFile(outputFile, jenkinsReportTemplate, JenkinsReportParams{Params: parameters, JenkinsBaseURL: opts.endpoint, JobNamesToRatings: jobNamesToRatings})

	writeJSONToOutputFile(strings.TrimSuffix(outputFile, ".html")+".json", parameters)
}

func writeHTMLReportToOutputFile(outputFile string, reportTemplate string, params interface{}) {
	reportOutputWriter := createReportOutputWriter(outputFile)
	defer reportOutputWriter.Close()

	err := flakefinder.WriteTemplateToOutput(reportTemplate, params, reportOutputWriter)
	if err != nil {
		jLog.Fatalf("failed to write report: %v", err)
	}
}

func writeJSONToOutputFile(jsonOutputFile string, parameters flakefinder.Params) {
	reportOutputWriter := createReportOutputWriter(jsonOutputFile)
	defer reportOutputWriter.Close()

	encoder := json.NewEncoder(reportOutputWriter)
	err := encoder.Encode(
		map[string]interface{}{
			"data":            parameters.Data,
			"failuresForJobs": parameters.FailuresForJobs,
		},
	)
	if err != nil {
		jLog.Fatalf("failed to write report: %v", err)
	}
}

func createReportOutputWriter(outputFile string) *os.File {
	reportOutputWriter, err := os.OpenFile(outputFile, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil && err != os.ErrNotExist {
		jLog.Fatalf("failed to write report: %v", err)
	}
	return reportOutputWriter
}
