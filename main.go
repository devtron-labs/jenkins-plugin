package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bndr/gojenkins"
	"github.com/caarlos0/env"
	_ "github.com/devtron-labs/common-lib/pubsub-lib"
	"os"
	"strings"
	"sync"
	"time"
)

type JenkinsTerminalStatus = string

const (
	SUCCESS JenkinsTerminalStatus = "SUCCESS"
	FAILURE                       = "FAILURE"
	ABORTED                       = "ABORTED"
)

// git material request corresponding to trigger
const (
	GIT_MATERIAL_REPO          string = "GIT_MATERIAL_REPO"
	GIT_MATERIAL_CHECKOUT_PATH string = "GIT_MATERIAL_CHECKOUT_PATH"
	GIT_MATERIAL_BRANCH        string = "GIT_MATERIAL_BRANCH"
	GIT_MATERIAL_COMMIT_HASH   string = "GIT_MATERIAL_COMMIT_HASH"
)

const (
	depth int = 1 // used for calling jenkins api
)

const (
	TIMED_OUT_ERROR = "timed-out"
)

type JenkinsPluginInputVariables struct {
	Url                     string `env:"URL"`
	Username                string `env:"USERNAME"`
	Password                string `env:"PASSWORD"`
	JobName                 string `env:"JOB_NAME"`
	JobTriggerParams        string `env:"JOB_TRIGGER_PARAMS"`
	JenkinsPluginTimeout    int    `env:"JENKINS_PLUGIN_TIMEOUT" envDefault:"30"`
	BuildStatusPollDuration int    `env:"BUILD_STATUS_POLL_DURATION" envDefault:"1"`
	GitMaterialRequest      string `env:"GIT_MATERIAL_REQUEST"`
}

var jenkinsRequest *JenkinsPluginInputVariables

func main() {

	jenkinsRequest = &JenkinsPluginInputVariables{}
	err := env.Parse(jenkinsRequest)
	if err != nil {
		fmt.Println("Error in parsing input variable of jenkins plugin", "err", err)
		return
	}

	inputParams := getTriggerApiInputParams()

	ctx := context.Background()

	fmt.Println("Step-1: Creating jenkins client")
	jenkins := gojenkins.CreateJenkins(nil, jenkinsRequest.Url, jenkinsRequest.Username, jenkinsRequest.Password)

	_, err = jenkins.Init(ctx)
	if err != nil {
		fmt.Println("initial connection attempt failed - Retrying connection")
		_, retryErr := jenkins.Init(ctx)
		if retryErr != nil {
			fmt.Println("error in creating jenkins client, please make sure jenkins server is running and credentials are valid")
			panic("client creation failed")
		}
	}
	fmt.Println("-----------------Jenkins client successfully created-----------------")

	fmt.Printf("Step 2: Triggering jenkins job: %s \n", jenkinsRequest.JobName)
	queueId, err := jenkins.BuildJob(ctx, jenkinsRequest.JobName, inputParams)
	if err != nil {
		fmt.Printf("error in trigger build - err : %s \n", err)
	}

	if queueId == 0 && err == nil {
		fmt.Sprintf("exiting as job is already running")
		os.Exit(0)
	}
	if err != nil {
		panic(err)
	}

	build, err := jenkins.GetBuildFromQueueID(ctx, queueId)
	if err != nil {
		panic(err)
	}
	fmt.Println(`Job build no - `, build.Raw.ID)

	jenkinsPluginTimeout := time.Minute * time.Duration(jenkinsRequest.JenkinsPluginTimeout)
	ctx, _ = context.WithTimeout(context.Background(), jenkinsPluginTimeout)

	wg := new(sync.WaitGroup)
	wg.Add(2)

	go func() {
		err = pollBuildStatus(ctx, wg, build)
		if err != nil {
			panic(err)
		}
	}()

	go func() {
		err = printBuildLogs(ctx, wg, build)
		if err != nil {
			panic(err)
		}
	}()

	wg.Wait()

	var jobStatus string
	if build.Raw.Result == "" {
		jobStatus = "Unknown"
	} else {
		jobStatus = build.Raw.Result
	}
	fmt.Println("Job execution completed ")
	fmt.Printf("Jenkins job build final status - %s\n", jobStatus)
}

func getTriggerApiInputParams() map[string]string {

	inputParamsMap := make(map[string]string)
	if len(jenkinsRequest.JobTriggerParams) > 0 {
		_ = json.Unmarshal([]byte(jenkinsRequest.JobTriggerParams), &inputParamsMap)
	}

	// if user has given "GIT_MATERIAL_REPO", "GIT_MATERIAL_CHECKOUT_PATH", "GIT_MATERIAL_BRANCH" or "GIT_MATERIAL_COMMIT_HASH" in input parameters
	// then it's value is parsed with original value
	gitMaterialRequest := jenkinsRequest.GitMaterialRequest
	//handling only for one git repo
	gitMaterialDetailsForFirstRepo := strings.Split(gitMaterialRequest, "|") // GIT_MATERIAL_REQUEST will be of form "<repoName1>,<checkoutPath1>,<BranchName1>,<CommitHash1>|<repoName2>,<checkoutPath2>,<BranchName2>,<CommitHash2>"
	gitMaterialDetails := strings.Split(gitMaterialDetailsForFirstRepo[0], ",")
	gitMaterialDetailsMap := make(map[string]string)
	if len(gitMaterialDetails) == 4 {
		gitMaterialDetailsMap[GIT_MATERIAL_REPO] = gitMaterialDetails[0]
		gitMaterialDetailsMap[GIT_MATERIAL_CHECKOUT_PATH] = gitMaterialDetails[1]
		gitMaterialDetailsMap[GIT_MATERIAL_BRANCH] = gitMaterialDetails[2]
		gitMaterialDetailsMap[GIT_MATERIAL_COMMIT_HASH] = gitMaterialDetails[3]
	}

	InputParamsWithVariableValueResolved := make(map[string]string)
	for k, v := range inputParamsMap {
		k = strings.Trim(k, " ")
		v = strings.Trim(v, " ")
		variableValue, ok := gitMaterialDetailsMap[v]
		if ok {
			InputParamsWithVariableValueResolved[k] = variableValue
		} else {
			InputParamsWithVariableValueResolved[k] = v
		}
	}

	return InputParamsWithVariableValueResolved
}

func pollBuildStatus(ctx context.Context, wg *sync.WaitGroup, build *gojenkins.Build) error {
	defer wg.Done()

	for !isTerminalStatus(build.Raw.Result) {
		select {
		case <-ctx.Done():
			fmt.Printf("plugin timeout occured")
			return errors.New(TIMED_OUT_ERROR)
		default:
			buildStatusPollDuration := time.Minute * time.Duration(jenkinsRequest.BuildStatusPollDuration)
			time.Sleep(buildStatusPollDuration)
			_, err := build.Poll(ctx, depth)
			if err != nil {
				fmt.Printf("error in polling build status")
				return err
			}
			return nil
		}
	}
	return nil
}

func isTerminalStatus(buildResult string) bool {
	switch buildResult {
	case SUCCESS:
		return true
	case FAILURE:
		return true
	case ABORTED:
		return true
	}
	return false
}

func printBuildLogs(ctx context.Context, wg *sync.WaitGroup, build *gojenkins.Build) error {
	defer wg.Done()

	var startIndex int64 = 0
	hasMoreText := true

	for hasMoreText {
		select {
		case <-ctx.Done():
			fmt.Println("plugin timeout occured")
			return errors.New(TIMED_OUT_ERROR)
		default:
			consoleResponse, err := build.GetConsoleOutputFromIndex(ctx, startIndex)
			if err != nil {
				fmt.Println(err)
				return err
			}
			if len(consoleResponse.Content) > 0 {
				fmt.Printf(consoleResponse.Content)
			}
			hasMoreText = consoleResponse.HasMoreText
			startIndex = consoleResponse.Offset
		}
	}
	return nil
}
