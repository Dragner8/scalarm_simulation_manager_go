package main

// TODO CPU type and MHz monitoring
// TODO getting random experiment id when there is no one in the config

import (
	"archive/zip"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	// "runtime"
	"math/rand"
	"strings"
	"time"
)

// Config file description - this should be provided by Experiment Manager in 'config.json'
type SimulationManagerConfig struct {
	ExperimentId           string `json:"experiment_id"`
	InformationServiceUrl  string `json:"information_service_url"`
	ExperimentManagerUser  string `json:"experiment_manager_user"`
	ExperimentManagerPass  string `json:"experiment_manager_pass"`
	Development            bool   `json:"development"`
	StartAt                string `json:"start_at"`
	Timeout                int    `json:"timeout"`
	ScalarmCertificatePath string `json:"scalarm_certificate_path"`
	InsecureSSL            bool   `json:"insecure_ssl"`
}

// Results structure - we send this back to Experiment Manager
type SimulationRunResults struct {
	Status  string      `json:"status"`
	Results interface{} `json:"results"`
	Reason  string      `json:"reason"`
}

type RequestInfo struct {
	HttpMethod    string
	Body          io.Reader
	ContentType   string
	ServiceMethod string
}

func Fatal(err error) {
	fmt.Println("[Fatal error] %s\n", err.Error())
	os.Exit(1)
}

func PrintStdoutLog() {
	linesNum := "100" // TODO: make int strconv.Itoa(linesNum)
	stdoutPath := "_stdout.txt"
	out, _ := exec.Command("tail", "-n", linesNum, stdoutPath).CombinedOutput()
	fmt.Printf("----------\nLast %v lines of %v:\n----------\n", linesNum, stdoutPath)
	fmt.Println(string(out))
}

func cloneZipItem(f *zip.File, dest string) error {
	//create full directory path
	path := filepath.Join(dest, f.Name)

	err := os.MkdirAll(filepath.Dir(path), os.ModeDir|os.ModePerm)
	if err != nil {
		return err
	}

	//clone if item is a file
	rc, err := f.Open()
	if err != nil {
		return err
	}

	if !f.FileInfo().IsDir() {

		fileCopy, err := os.Create(path)
		if err != nil {
			return err
		}

		_, err = io.Copy(fileCopy, rc)
		fileCopy.Close()
		if err != nil {
			return err
		}
	}
	rc.Close()
	return nil
}

func Extract(zip_path, dest string) error {
	r, err := zip.OpenReader(zip_path)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		err = cloneZipItem(f, dest)
		if err != nil {
			return err
		}
	}

	return nil
}

func ExecuteScalarmRequest(reqInfo RequestInfo, serviceUrls []string, config *SimulationManagerConfig,
	client *http.Client, timeout time.Duration) []byte {

	protocol := "https"
	if config.Development {
		protocol = "http"
	}

	// 1. shuffle service url
	perm := rand.Perm(len(serviceUrls))

	for _, v := range perm {
		// 2. get next service url and prepare a request
		serviceUrl := serviceUrls[v]
		fmt.Printf("[SiM] %s://%s/%s\n", protocol, serviceUrl, reqInfo.ServiceMethod)
		req, err := http.NewRequest(reqInfo.HttpMethod, fmt.Sprintf("%s://%s/%s", protocol, serviceUrl, reqInfo.ServiceMethod), reqInfo.Body)
		if err != nil {
			Fatal(err)
		}
		req.SetBasicAuth(config.ExperimentManagerUser, config.ExperimentManagerPass)
		if reqInfo.Body != nil {
			req.Header.Set("Content-Type", reqInfo.ContentType)
		}
		// 3. execute request with timeout
		response, err := GetWithTimeout(client, req, timeout)
		// 4. if response body is nil go to 2.
		if err == nil {
			return response
		}
	}

	Fatal(fmt.Errorf("Could not execute request against Scalarm service"))
	return nil
}

// Calling Get multiple time until valid response or exceed 'communicationTimeout' period
func GetWithTimeout(client *http.Client, request *http.Request, communicationTimeout time.Duration) ([]byte, error) {
	var resp *http.Response
	var err error
	communicationFailed := true
	communicationStart := time.Now()
	var body []byte

	for communicationStart.Add(communicationTimeout).After(time.Now()) {
		resp, err = client.Do(request)

		if err != nil {
			time.Sleep(1 * time.Second)
			fmt.Printf("[SiM] %v\n", err)
		} else {
			communicationFailed = false
			break
		}
	}

	if communicationFailed {
		return nil, err
	}

	defer resp.Body.Close()

	if body, err = ioutil.ReadAll(resp.Body); err != nil {
		return nil, err
	}

	return body, nil
}

// this method executes progress monitor of a simulation run and stops when it gets a signal from the main thread
func IntermediateMonitoring(messages chan struct{}, finished chan struct{}, codeBaseDir string, experimentManagers []string, simIndex float64,
	config *SimulationManagerConfig, simulationDirPath string, client *http.Client) {

	communicationTimeout := 30 * time.Second

	if _, err := os.Stat(path.Join(codeBaseDir, "progress_monitor")); err == nil {
		for {
			progressMonitorCmd := exec.Command("sh", "-c", path.Join(codeBaseDir, "progress_monitor >>_stdout.txt 2>&1"))
			progressMonitorCmd.Dir = simulationDirPath

			if err = progressMonitorCmd.Run(); err != nil {
				fmt.Println("[SiM] An error occurred during 'progress_monitor' execution.")
				fmt.Println("[SiM] Please check if 'progress_monitor' executes correctly on the selected infrastructure.")
				fmt.Printf("[Fatal error] occured during '%v' execution \n", strings.Join(progressMonitorCmd.Args, " "))
				fmt.Printf("[Fatal error] %s\n", err.Error())
				PrintStdoutLog()
				os.Exit(1)
			}

			intermediateResults := new(SimulationRunResults)

			if _, err := os.Stat("intermediate_result.json"); os.IsNotExist(err) {
				intermediateResults.Status = "error"
				intermediateResults.Reason = fmt.Sprintf("No 'intermediate_result.json' file found: %s", err.Error())
			} else {
				file, err := os.Open("intermediate_result.json")

				if err != nil {
					intermediateResults.Status = "error"
					intermediateResults.Reason = fmt.Sprintf("Could not open 'intermediate_result.json': %s", err.Error())
				} else {
					err = json.NewDecoder(file).Decode(&intermediateResults)

					if err != nil {
						intermediateResults.Status = "error"
						intermediateResults.Reason = fmt.Sprintf("Error during 'intermediate_result.json' parsing: %s", err.Error())
					}
				}

				file.Close()
			}

			if intermediateResults.Status == "ok" {
				data := url.Values{}
				data.Set("status", intermediateResults.Status)
				data.Add("reason", intermediateResults.Reason)
				b, _ := json.Marshal(intermediateResults.Results)
				data.Add("result", string(b))

				fmt.Printf("[SiM][progress_info] Results: %v\n", data)

				progressInfo := RequestInfo{"POST", strings.NewReader(data.Encode()),
					"application/x-www-form-urlencoded",
					fmt.Sprintf("experiments/%v/simulations/%v/progress_info", config.ExperimentId, simIndex)}

				body := ExecuteScalarmRequest(progressInfo, experimentManagers, config, client, communicationTimeout)

				fmt.Printf("[SiM][progress_info] Response body: %s\n", body)
			}

			time.Sleep(10 * time.Second)
			select {
			case _ = <-messages:
				fmt.Printf("[SiM][progress_info] Our work is finished\n")
				finished <- struct{}{}
				return
			default:
			}
		}
	} else {
		fmt.Printf("[SiM][progress_info] There is no progress monitor script\n")
		finished <- struct{}{}
	}
}

func main() {
	var file *os.File
	var experimentDir string

	rand.Seed(time.Now().UTC().UnixNano())

	// 0. remember current location
	rootDirPath, _ := os.Getwd()
	rootDir, err := os.Open(rootDirPath)
	if err != nil {
		Fatal(err)
	}

	fmt.Printf("[SiM] working directory: %s\n", rootDirPath)

	// 1. load config file
	configFile, err := os.Open("config.json")
	if err != nil {
		Fatal(err)
	}

	config := new(SimulationManagerConfig)
	err = json.NewDecoder(configFile).Decode(&config)
	configFile.Close()

	if err != nil {
		Fatal(err)
	}

	if config.Timeout <= 0 {
		config.Timeout = 60
	}
	communicationTimeout := time.Duration(config.Timeout) * time.Second

	// -- HTTP client --

	var client *http.Client
	tlsConfig := tls.Config{InsecureSkipVerify: config.InsecureSSL}

	if config.ScalarmCertificatePath != "" {
		CA_Pool := x509.NewCertPool()
		severCert, err := ioutil.ReadFile(config.ScalarmCertificatePath)
		if err != nil {
			Fatal(fmt.Errorf("Could not load Scalarm certificate"))
		}
		CA_Pool.AppendCertsFromPEM(severCert)

		tlsConfig.RootCAs = CA_Pool
	}

	client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tlsConfig}}

	// --

	if len(config.StartAt) > 0 {
		startTime, err := time.Parse(time.RFC3339, config.StartAt)
		if err != nil {
			fmt.Printf("[SiM] %v\n", err)
		} else {
			fmt.Println("[SiM] We have start_at provided")
			time.Sleep(startTime.Sub(time.Now()))
			fmt.Println("[SiM] We are ready to work")
		}
	}

	//2. getting experiment and storage manager addresses
	iSReqInfo := RequestInfo{"GET", nil, "", "experiment_managers"}
	body := ExecuteScalarmRequest(iSReqInfo, []string{config.InformationServiceUrl}, config, client, communicationTimeout)

	var experimentManagers []string

	fmt.Printf("[SiM] Response body: %s.\n", body)

	if err := json.Unmarshal(body, &experimentManagers); err != nil {
		Fatal(err)
	}

	if len(experimentManagers) == 0 {
		Fatal(fmt.Errorf("There is no Experiment Manager registered in Information Service. Please contact Scalarm administrators."))
	}

	// getting storage manager address
	iSReqInfo = RequestInfo{"GET", nil, "", "storage_managers"}
	body = ExecuteScalarmRequest(iSReqInfo, []string{config.InformationServiceUrl}, config, client, communicationTimeout)

	var storageManagers []string

	fmt.Printf("[SiM] Response body: %s.\n", body)

	if err := json.Unmarshal(body, &storageManagers); err != nil {
		Fatal(err)
	}

	if len(storageManagers) == 0 {
		Fatal(fmt.Errorf("There is no Storage Manager registered in Information Service. Please contact Scalarm administrators."))
	}

	// creating directory for experiment data
	experimentDir = path.Join(rootDirPath, fmt.Sprintf("experiment_%s", config.ExperimentId))

	if err = os.MkdirAll(experimentDir, 0777); err != nil {
		Fatal(err)
	}

	// 3. get code base for the experiment if necessary
	codeBaseDir := path.Join(experimentDir, "code_base")

	if _, err := os.Stat(codeBaseDir); os.IsNotExist(err) {
		if err = os.MkdirAll(codeBaseDir, 0777); err != nil {
			Fatal(err)
		}
		fmt.Println("[SiM] Getting code base ...")
		codeBaseUrl := fmt.Sprintf("experiments/%s/code_base", config.ExperimentId)
		codeBaseInfo := RequestInfo{"GET", nil, "", codeBaseUrl}
		body = ExecuteScalarmRequest(codeBaseInfo, experimentManagers, config, client, communicationTimeout)

		w, err := os.Create(path.Join(codeBaseDir, "code_base.zip"))
		if err != nil {
			Fatal(err)
		}
		defer w.Close()

		if _, err = io.Copy(w, bytes.NewReader(body)); err != nil {
			Fatal(err)
		}

		if err = Extract(codeBaseDir+"/code_base.zip", codeBaseDir); err != nil {
			fmt.Println("[SiM] An error occurred while unzipping 'code_base.zip'.")
			fmt.Println("[Fatal error] occured while unzipping 'code_base.zip'.")
			fmt.Printf("[Fatal error] %s\n", err.Error())
			os.Exit(2)
		}
		if err = Extract(codeBaseDir+"/simulation_binaries.zip", codeBaseDir); err != nil {
			fmt.Println("[SiM] An error occurred while unzipping 'simulation_binaries.zip'.")
			fmt.Println("[Fatal error] occured while unzipping 'simulation_binaries.zip'.")
			fmt.Printf("[Fatal error] %s\n", err.Error())
			os.Exit(2)
		}

		if err = exec.Command("sh", "-c", fmt.Sprintf("chmod a+x \"%s\"/*", codeBaseDir)).Run(); err != nil {
			fmt.Println("[SiM] An error occurred during executing 'chmod' command. Please check if you have required permissions.")
			fmt.Printf("[Fatal error] occured during '%v' execution \n", fmt.Sprintf("chmod a+x \"%s\"/*", codeBaseDir))
			fmt.Printf("[Fatal error] %s\n", err.Error())
			os.Exit(2)
		}
	}

	// 4. main loop for getting simulation runs of an experiment
	for {
		nextSimulationFailed := true
		communicationStart := time.Now()

		var nextSimulationBody []byte
		var simulation_run map[string]interface{}
		wait := false

		// 4.a getting input values for next simulation run
		for communicationStart.Add(communicationTimeout * time.Duration(len(experimentManagers))).After(time.Now()) {
			fmt.Println("[SiM] Getting next simulation run ...")
			nextSimulationUrl := fmt.Sprintf("experiments/%s/next_simulation", config.ExperimentId)
			nextSimulationInfo := RequestInfo{"GET", nil, "", nextSimulationUrl}
			nextSimulationBody = ExecuteScalarmRequest(nextSimulationInfo, experimentManagers, config, client, communicationTimeout)

			fmt.Printf("[SiM] Next simulation: %s\n", nextSimulationBody)

			if err = json.Unmarshal(nextSimulationBody, &simulation_run); err != nil {
				fmt.Printf("[SiM] %v\n", err)
			} else {
				status := simulation_run["status"].(string)

				if status == "all_sent" {
					fmt.Println("[SiM] There is no more simulations to run in this experiment.")
				} else if status == "error" {
					fmt.Println("[SiM] An error occurred while getting next simulation.")
				} else if status == "wait" {
					fmt.Printf("[SiM] There is no more simulations to run in this experiment "+
						"at the moment, time to wait: %vs\n", simulation_run["duration_in_seconds"])
					wait = true
					break
				} else if status != "ok" {
					fmt.Println("[SiM] We cannot continue due to unsupported status.")
				} else {
					nextSimulationFailed = false
					break
				}
			}

			fmt.Println("[SiM] There was a problem while getting next simulation to run.")
			time.Sleep(5 * time.Second)
		}
		if wait {
			time.Sleep(time.Duration(simulation_run["duration_in_seconds"].(float64)) * time.Second)
			continue
		}

		if nextSimulationFailed {
			fmt.Println("[SiM] Couldn't get simulation to run -> finishing work.")
			os.Exit(0)
		}

		simulation_index := simulation_run["simulation_id"].(float64)

		fmt.Printf("[SiM] Simulation index: %v\n", simulation_index)
		fmt.Printf("[SiM] Simulation execution constraints: %v\n", simulation_run["execution_constraints"])

		simulationDirPath := path.Join(experimentDir, fmt.Sprintf("simulation_%v", simulation_index))

		err = os.MkdirAll(simulationDirPath, 0777)
		if err != nil {
			Fatal(err)
		}

		input_parameters, _ := json.Marshal(simulation_run["input_parameters"].(map[string]interface{}))

		err = ioutil.WriteFile(path.Join(simulationDirPath, "input.json"), input_parameters, 0777)
		if err != nil {
			Fatal(err)
		}

		simulationDir, err := os.Open(simulationDirPath)
		if err != nil {
			Fatal(err)
		}

		wd, err := os.Getwd()
		fmt.Printf("[SiM] Working dir: %v\n", wd)
		if err = simulationDir.Chdir(); err != nil {
			Fatal(err)
		}
		wd, err = os.Getwd()

		// 4b. run an adapter script (input writer) for input information: input.json -> some specific code
		if _, err := os.Stat(path.Join(codeBaseDir, "input_writer")); err == nil {
			fmt.Println("[SiM] Before input writer ...")
			inputWriterCmd := exec.Command("sh", "-c", path.Join(codeBaseDir, "input_writer input.json >>_stdout.txt 2>&1"))
			inputWriterCmd.Dir = simulationDirPath
			if err = inputWriterCmd.Run(); err != nil {
				fmt.Println("[SiM] An error occurred during 'input_writer' execution.")
				fmt.Println("[SiM] Please check if 'input_writer' executes correctly on the selected infrastructure.")
				fmt.Printf("[Fatal error] occured during '%v' execution \n", strings.Join(inputWriterCmd.Args, " "))
				fmt.Printf("[Fatal error] %s\n", err.Error())
				PrintStdoutLog()
				os.Exit(1)
			}
			fmt.Println("[SiM] After input writer ...")
		}

		// 4c.1. progress monitoring scheduling if available - TODO
		messages := make(chan struct{}, 1)
		finished := make(chan struct{}, 1)
		go IntermediateMonitoring(messages, finished, codeBaseDir, experimentManagers, simulation_index, config, simulationDirPath, client)

		// 4c. run an executor of this simulation
		fmt.Println("[SiM] Before executor ...")
		executorCmd := exec.Command("sh", "-c", path.Join(codeBaseDir, "executor >>_stdout.txt 2>&1"))
		executorCmd.Dir = simulationDirPath
		if err = executorCmd.Run(); err != nil {
			fmt.Println("[SiM] An error occurred during 'executor' execution.")
			fmt.Println("[SiM] Please check if 'executor' executes correctly on the selected infrastructure.")
			fmt.Printf("[Fatal error] occured during '%v' execution \n", strings.Join(executorCmd.Args, " "))
			fmt.Printf("[Fatal error] %s\n", err.Error())
			PrintStdoutLog()
			os.Exit(1)
		}
		fmt.Println("[SiM] After executor ...")

		messages <- struct{}{}
		close(messages)

		// 4d. run an adapter script (output reader) to transform specific output format to scalarm model (output.json)
		if _, err := os.Stat(path.Join(codeBaseDir, "output_reader")); err == nil {
			fmt.Println("[SiM] Before output reader ...")
			outputReaderCmd := exec.Command("sh", "-c", path.Join(codeBaseDir, "output_reader >>_stdout.txt 2>&1"))
			outputReaderCmd.Dir = simulationDirPath
			if err = outputReaderCmd.Run(); err != nil {
				fmt.Println("[SiM] An error occurred during 'output_reader' execution.")
				fmt.Println("[SiM] Please check if 'output_reader' executes correctly on the selected infrastructure.")
				fmt.Printf("[Fatal error] occured during '%v' execution \n", strings.Join(outputReaderCmd.Args, " "))
				fmt.Printf("[Fatal error] %s\n", err.Error())	
				PrintStdoutLog()
				os.Exit(1)
			}
			fmt.Println("[SiM] After output reader ...")
		}

		// 4e. upload output json to experiment manager and set the run simulation as done
		simulationRunResults := new(SimulationRunResults)

		if _, err := os.Stat("output.json"); os.IsNotExist(err) {
			simulationRunResults.Status = "error"
			simulationRunResults.Reason = fmt.Sprintf("No output.json file found: %s", err.Error())
		} else {
			file, err = os.Open("output.json")

			if err != nil {
				simulationRunResults.Status = "error"
				simulationRunResults.Reason = fmt.Sprintf("Could not open output.json: %s", err.Error())
			} else {
				err = json.NewDecoder(file).Decode(&simulationRunResults)

				if err != nil {
					simulationRunResults.Status = "error"
					simulationRunResults.Reason = fmt.Sprintf("Error during output.json parsing: %s", err.Error())
				}
			}

			file.Close()
		}

		// 4f. upload structural results of a simulation run
		data := url.Values{}
		data.Set("status", simulationRunResults.Status)
		data.Add("reason", simulationRunResults.Reason)
		b, _ := json.Marshal(simulationRunResults.Results)
		data.Add("result", string(b))

		fmt.Printf("[SiM] Results: %v\n", data)

		markAsCompleteUrl := fmt.Sprintf("experiments/%s/simulations/%v/mark_as_complete", config.ExperimentId, simulation_index)
		markAsCompleteInfo := RequestInfo{"POST", strings.NewReader(data.Encode()), "application/x-www-form-urlencoded",
			markAsCompleteUrl}
		body = ExecuteScalarmRequest(markAsCompleteInfo, experimentManagers, config, client, communicationTimeout)

		fmt.Printf("[SiM] Response body: %s\n", body)

		// 4g. upload binary output if provided
		if _, err := os.Stat("output.tar.gz"); err == nil {
			fmt.Printf("[SiM] Uploading 'output.tar.gz' ...\n")
			file, err := os.Open("output.tar.gz")

			if err != nil {
				Fatal(err)
			}

			defer file.Close()

			requestBody := &bytes.Buffer{}
			writer := multipart.NewWriter(requestBody)
			part, err := writer.CreateFormFile("file", filepath.Base("output.tar.gz"))
			if err != nil {
				Fatal(err)
			}
			_, err = io.Copy(part, file)

			err = writer.Close()
			if err != nil {
				Fatal(err)
			}

			binariesUploadUrl := fmt.Sprintf("experiments/%s/simulations/%v", config.ExperimentId, simulation_index)
			binariesUploadUrlInfo := RequestInfo{"PUT", requestBody, writer.FormDataContentType(), binariesUploadUrl}
			body = ExecuteScalarmRequest(binariesUploadUrlInfo, storageManagers, config, client, communicationTimeout)

			fmt.Printf("[SiM] Response body: %s\n", body)
		}

		// 4h. upload stdout if provided
		if _, err := os.Stat("_stdout.txt"); err == nil {
			fmt.Println("[SiM] Uploading STDOUT of the simulation run ...")

			file, err := os.Open("_stdout.txt")
			if err != nil {
				Fatal(err)
			}

			requestBody := &bytes.Buffer{}
			writer := multipart.NewWriter(requestBody)
			part, err := writer.CreateFormFile("file", filepath.Base("_stdout.txt"))
			if err != nil {
				Fatal(err)
			}
			_, err = io.Copy(part, file)
			file.Close()

			err = writer.Close()
			if err != nil {
				Fatal(err)
			}

			stdoutUploadUrl := fmt.Sprintf("experiments/%s/simulations/%v/stdout", config.ExperimentId, simulation_index)
			stdoutUploadUrlInfo := RequestInfo{"PUT", requestBody, writer.FormDataContentType(), stdoutUploadUrl}
			body = ExecuteScalarmRequest(stdoutUploadUrlInfo, storageManagers, config, client, communicationTimeout)

			fmt.Printf("[SiM] Response body: %s\n", body)
		}

		// 5. clean up - removing simulation dir
		go func() {
			select {
			case _ = <-finished:
				os.RemoveAll(simulationDirPath)
				close(finished)
			}
		}()

		// 6. going to the root dir and moving
		if err = rootDir.Chdir(); err != nil {
			Fatal(err)
		}
	}
}