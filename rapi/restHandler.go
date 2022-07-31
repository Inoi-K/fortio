// Copyright 2022 Fortio Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Remote API to trigger load tests package (REST API).
package rapi // import "fortio.org/fortio/rapi"

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"fortio.org/fortio/bincommon"
	"fortio.org/fortio/fgrpc"
	"fortio.org/fortio/fhttp"
	"fortio.org/fortio/jrpc"
	"fortio.org/fortio/log"
	"fortio.org/fortio/periodic"
	"fortio.org/fortio/stats"
	"fortio.org/fortio/tcprunner"
	"fortio.org/fortio/udprunner"
)

const (
	restRunURI    = "rest/run"
	restStatusURI = "rest/status"
	restStopURI   = "rest/stop"
	ModeGRPC      = "grpc"
)

var (
	uiRunMapMutex = &sync.Mutex{}
	id            int64
	runs          = make(map[int64]*periodic.RunnerOptions)
	// Directory where results are written to/read from.
	dataDir string
	// Default percentiles when not otherwise specified.
	DefaultPercentileList []float64
)

// AsyncReply is returned when async=on is passed.
type AsyncReply struct {
	jrpc.ServerReply
	RunID int64
	Count int
}

type StatusReply struct {
	AsyncReply
	Runs []*periodic.RunnerOptions
}

// Error writes serialized ServerReply marked as error, to the writer.
func Error(w http.ResponseWriter, msg string, err error) {
	if w == nil {
		// async mode, nothing to do
		return
	}
	_ = jrpc.ReplyError(w, msg, err)
}

// GetConfigAtPath deserializes the bytes as JSON and
// extracts the map at the given path (only supports simple expression:
// . is all the json
// .foo.bar.blah will extract that part of the tree.
func GetConfigAtPath(path string, data []byte) (map[string]interface{}, error) {
	// that's what Unmarshal does anyway if you pass interface{} var, skips a cast even for dynamic/unknown json
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return getConfigAtPath(path, m)
}

// recurse on the requested path.
func getConfigAtPath(path string, m map[string]interface{}) (map[string]interface{}, error) {
	path = strings.TrimLeft(path, ".")
	if path == "" {
		return m, nil
	}
	parts := strings.SplitN(path, ".", 2)
	log.Debugf("split got us %v", parts)
	first := parts[0]
	rest := ""
	if len(parts) == 2 {
		rest = parts[1]
	}
	nm, found := m[first]
	if !found {
		return nil, fmt.Errorf("%q not found in json", first)
	}
	mm, ok := nm.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%q path is not a map", first)
	}
	return getConfigAtPath(rest, mm)
}

// FormValue gets the value from the query arguments/url parameter or from the
// provided map (json data).
func FormValue(r *http.Request, json map[string]interface{}, key string) string {
	// query args have priority
	res := r.FormValue(key)
	if res != "" {
		log.Debugf("key %q in query args so using that value %q", key, res)
		return res
	}
	res2, found := json[key]
	if !found {
		log.Debugf("key %q not in json map", key)
		return ""
	}
	res, ok := res2.(string)
	if !ok {
		log.Warnf("%q is %+v / not a string, can't be used", key, res2)
		return ""
	}
	log.LogVf("Getting %q from json: %q", key, res)
	return res
}

// RESTRunHandler is api version of UI submit handler.
func RESTRunHandler(w http.ResponseWriter, r *http.Request) { // nolint: funlen
	fhttp.LogRequest(r, "REST Run Api call")
	w.Header().Set("Content-Type", "application/json")
	data, err := ioutil.ReadAll(r.Body) // must be done before calling FormValue
	if err != nil {
		log.Errf("Error reading %v", err)
		Error(w, "body read error", err)
		return
	}
	log.Infof("REST body: %s", fhttp.DebugSummary(data, 250))
	jsonPath := r.FormValue("jsonPath")
	var jd map[string]interface{}
	if len(data) > 0 {
		// Json input and deserialize options from that path, eg. for flagger:
		// jsonPath=.metadata
		jd, err = GetConfigAtPath(jsonPath, data)
		if err != nil {
			log.Errf("Error deserializing %v", err)
			Error(w, "body json deserialization error", err)
			return
		}
		log.Infof("Body: %+v", jd)
	}
	url := FormValue(r, jd, "url")
	runner := FormValue(r, jd, "runner")
	if runner == "" {
		runner = "http"
	}
	log.Infof("Starting API run %s load request from %v for %s", runner, r.RemoteAddr, url)
	async := (FormValue(r, jd, "async") == "on")
	payload := FormValue(r, jd, "payload")
	labels := FormValue(r, jd, "labels")
	resolution, _ := strconv.ParseFloat(FormValue(r, jd, "r"), 64)
	percList, _ := stats.ParsePercentiles(FormValue(r, jd, "p"))
	qps, _ := strconv.ParseFloat(FormValue(r, jd, "qps"), 64)
	durStr := FormValue(r, jd, "t")
	jitter := (FormValue(r, jd, "jitter") == "on")
	uniform := (FormValue(r, jd, "uniform") == "on")
	nocatchup := (FormValue(r, jd, "nocatchup") == "on")
	stdClient := (FormValue(r, jd, "stdclient") == "on")
	sequentialWarmup := (FormValue(r, jd, "sequential-warmup") == "on")
	httpsInsecure := (FormValue(r, jd, "https-insecure") == "on")
	resolve := FormValue(r, jd, "resolve")
	timeoutStr := strings.TrimSpace(FormValue(r, jd, "timeout"))
	timeout, _ := time.ParseDuration(timeoutStr) // will be 0 if empty, which is handled by runner and opts
	var dur time.Duration
	if durStr == "on" {
		dur = -1
	} else if durStr != "" {
		dur, err = time.ParseDuration(durStr)
		if err != nil {
			log.Errf("Error parsing duration '%s': %v", durStr, err)
			Error(w, "parsing duration", err)
			return
		}
	}
	c, _ := strconv.Atoi(FormValue(r, jd, "c"))
	out := io.Writer(os.Stderr)
	if len(percList) == 0 && !strings.Contains(r.URL.RawQuery, "p=") {
		percList = DefaultPercentileList
	}
	n, _ := strconv.ParseInt(FormValue(r, jd, "n"), 10, 64)
	if strings.TrimSpace(url) == "" {
		Error(w, "URL is required", nil)
		return
	}
	ro := periodic.RunnerOptions{
		QPS:         qps,
		Duration:    dur,
		Out:         out,
		NumThreads:  c,
		Resolution:  resolution,
		Percentiles: percList,
		Labels:      labels,
		Exactly:     n,
		Jitter:      jitter,
		Uniform:     uniform,
		NoCatchUp:   nocatchup,
	}
	ro.Normalize()
	runid := AddRun(&ro)
	ro.RunID = runid
	log.Infof("New run id %d", runid)
	httpopts := &fhttp.HTTPOptions{}
	httpopts.HTTPReqTimeOut = timeout // to be normalized in init 0 replaced by default value
	httpopts = httpopts.Init(url)
	httpopts.ResetHeaders()
	httpopts.DisableFastClient = stdClient
	httpopts.SequentialWarmup = sequentialWarmup
	httpopts.Insecure = httpsInsecure
	httpopts.Resolve = resolve
	// Set the connection reuse range.
	err = bincommon.ConnectionReuseRange.
		WithValidator(bincommon.ConnectionReuseRangeValidator(httpopts)).
		Set(FormValue(r, jd, "connection-reuse"))
	if err != nil {
		log.Errf("Fail to validate connection reuse range flag, err: %v", err)
	}

	if len(payload) > 0 {
		httpopts.Payload = []byte(payload)
	}
	jsonHeaders, found := jd["headers"]
	for found { // really an if, but using while to break out without else below
		res, ok := jsonHeaders.([]interface{})
		if !ok {
			log.Warnf("Json Headers is %T %v / not an array, can't be used", jsonHeaders, jsonHeaders)
			break
		}
		for _, header := range res {
			log.LogVf("adding json header %T: %v", header, header)
			hStr, ok := header.(string)
			if !ok {
				log.Errf("Json headers must be an array of strings (got %T: %v)", header, header)
				continue
			}
			if err := httpopts.AddAndValidateExtraHeader(hStr); err != nil {
				log.Errf("Error adding custom json headers: %v", err)
			}
		}
		break
	}
	for _, header := range r.Form["H"] {
		if len(header) == 0 {
			continue
		}
		log.LogVf("adding query arg header %v", header)
		err := httpopts.AddAndValidateExtraHeader(header)
		if err != nil {
			log.Errf("Error adding custom query arg headers: %v", err)
		}
	}
	fhttp.OnBehalfOf(httpopts, r)
	if async {
		reply := AsyncReply{RunID: runid, Count: 1}
		reply.Message = "started" // nolint: goconst
		err := jrpc.ReplyOk(w, &reply)
		if err != nil {
			log.Errf("Error replying to start: %v", err)
		}
		go Run(nil, r, jd, runner, url, ro, httpopts)
		return
	}
	Run(w, r, jd, runner, url, ro, httpopts)
}

// Run executes the run (can be called async or not, writer is nil for async mode).
func Run(w http.ResponseWriter, r *http.Request, jd map[string]interface{},
	runner, url string, ro periodic.RunnerOptions, httpopts *fhttp.HTTPOptions,
) {
	//	go func() {
	var res periodic.HasRunnerResult
	var err error
	if runner == ModeGRPC { // nolint: nestif
		grpcSecure := (FormValue(r, jd, "grpc-secure") == "on")
		grpcPing := (FormValue(r, jd, "ping") == "on")
		grpcPingDelay, _ := time.ParseDuration(FormValue(r, jd, "grpc-ping-delay"))
		o := fgrpc.GRPCRunnerOptions{
			RunnerOptions: ro,
			Destination:   url,
			UsePing:       grpcPing,
			Delay:         grpcPingDelay,
		}
		o.TLSOptions = httpopts.TLSOptions
		if grpcSecure {
			o.Destination = fhttp.AddHTTPS(url)
		}
		// TODO: ReqTimeout: timeout
		res, err = fgrpc.RunGRPCTest(&o)
	} else if strings.HasPrefix(url, tcprunner.TCPURLPrefix) {
		// TODO: copy pasta from fortio_main
		o := tcprunner.RunnerOptions{
			RunnerOptions: ro,
		}
		o.ReqTimeout = httpopts.HTTPReqTimeOut
		o.Destination = url
		o.Payload = httpopts.Payload
		res, err = tcprunner.RunTCPTest(&o)
	} else if strings.HasPrefix(url, udprunner.UDPURLPrefix) {
		// TODO: copy pasta from fortio_main
		o := udprunner.RunnerOptions{
			RunnerOptions: ro,
		}
		o.ReqTimeout = httpopts.HTTPReqTimeOut
		o.Destination = url
		o.Payload = httpopts.Payload
		res, err = udprunner.RunUDPTest(&o)
	} else {
		o := fhttp.HTTPRunnerOptions{
			HTTPOptions:        *httpopts,
			RunnerOptions:      ro,
			AllowInitialErrors: true,
		}
		res, err = fhttp.RunHTTPTest(&o)
	}
	RemoveRun(ro.RunID)
	if err != nil {
		log.Errf("Init error for %s mode with url %s and options %+v : %v", runner, url, ro, err)
		Error(w, "Aborting because of error", err)
		return
	}
	json, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		log.Fatalf("Unable to json serialize result: %v", err)
	}
	id := res.Result().ID()
	doSave := (FormValue(r, jd, "save") == "on")
	if doSave {
		SaveJSON(id, json)
	}
	if w == nil {
		// async, no result to output
		return
	}
	_, err = w.Write(json)
	if err != nil {
		log.Errf("Unable to write json output for %v: %v", r.RemoteAddr, err)
	}
}

// RESTStatusHandler will print the state of the runs.
func RESTStatusHandler(w http.ResponseWriter, r *http.Request) {
	fhttp.LogRequest(r, "REST Status Api call")
	runid, _ := strconv.ParseInt(r.FormValue("runid"), 10, 64)
	statusReply := StatusReply{}
	statusReply.RunID = runid
	if runid != 0 {
		ro := GetRun(runid)
		if ro != nil {
			statusReply.Count = 1
			statusReply.Runs = []*periodic.RunnerOptions{ro}
		}
	} else {
		statusReply.Runs = GetAllRuns()
		statusReply.Count = len(statusReply.Runs)
	}
	err := jrpc.ReplyOk(w, &statusReply)
	if err != nil {
		log.Errf("Error replying to status: %v", err)
	}
}

// RESTStopHandler is the api to stop a given run by runid or all the runs if unspecified/0.
func RESTStopHandler(w http.ResponseWriter, r *http.Request) {
	fhttp.LogRequest(r, "REST Stop Api call")
	w.Header().Set("Content-Type", "application/json")
	runid, _ := strconv.ParseInt(r.FormValue("runid"), 10, 64)
	i := StopByRunID(runid)
	reply := AsyncReply{RunID: runid, Count: i}
	reply.Message = "stopped"
	err := jrpc.ReplyOk(w, &reply)
	if err != nil {
		log.Errf("Error replying: %v", err)
	}
}

// StopByRunID stops all the runs if passed 0 or the runid provided.
func StopByRunID(runid int64) int {
	uiRunMapMutex.Lock()
	if runid <= 0 { // Stop all
		i := 0
		for k, v := range runs {
			v.Abort()
			delete(runs, k)
			i++
		}
		uiRunMapMutex.Unlock()
		log.Infof("Interrupted all %d runs", i)
		return i
	}
	// else: Stop one
	v, found := runs[runid]
	if found {
		delete(runs, runid)
		uiRunMapMutex.Unlock()
		v.Abort()
		log.Infof("Interrupted run id %d", runid)
		return 1
	}
	log.Infof("Runid %d not found to interrupt", runid)
	uiRunMapMutex.Unlock()
	return 0
}

// AddHandlers adds the REST Api handlers for run, status and stop.
// uiPath must end with a /.
func AddHandlers(mux *http.ServeMux, uiPath, datadir string) {
	SetDataDir(datadir)
	restRunPath := uiPath + restRunURI
	mux.HandleFunc(restRunPath, RESTRunHandler)
	restStatusPath := uiPath + restStatusURI
	mux.HandleFunc(restStatusPath, RESTStatusHandler)
	restStopPath := uiPath + restStopURI
	mux.HandleFunc(restStopPath, RESTStopHandler)
	log.Printf("REST API on %s, %s, %s", restRunPath, restStatusPath, restStopPath)
}

// SaveJSON save Json bytes to give file name (.json) in data-path dir.
func SaveJSON(name string, json []byte) string {
	if dataDir == "" {
		log.Infof("Not saving because data-path is unset")
		return ""
	}
	name += ".json"
	log.Infof("Saving %s in %s", name, dataDir)
	err := ioutil.WriteFile(path.Join(dataDir, name), json, 0o644) // nolint: gosec // we do want 644
	if err != nil {
		log.Errf("Unable to save %s in %s: %v", name, dataDir, err)
		return ""
	}
	// Return the relative path from the /fortio/ UI
	return "data/" + name
}

func AddRun(ro *periodic.RunnerOptions) int64 {
	uiRunMapMutex.Lock()
	id++ // start at 1 as 0 means interrupt all
	runid := id
	runs[runid] = ro
	uiRunMapMutex.Unlock()
	return runid
}

func RemoveRun(id int64) {
	uiRunMapMutex.Lock()
	delete(runs, id)
	uiRunMapMutex.Unlock()
}

func GetRun(id int64) *periodic.RunnerOptions {
	uiRunMapMutex.Lock()
	res := runs[id]
	uiRunMapMutex.Unlock()
	return res
}

func GetAllRuns() []*periodic.RunnerOptions {
	res := []*periodic.RunnerOptions{}
	uiRunMapMutex.Lock()
	for _, v := range runs {
		res = append(res, v)
	}
	uiRunMapMutex.Unlock()
	return res
}

func SetDataDir(datadir string) {
	dataDir = datadir
}

func GetDataDir() string {
	return dataDir
}