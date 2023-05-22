// Copyright 2019 Richard Hartmann
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	webflag "github.com/prometheus/exporter-toolkit/web/kingpinflag"

	"github.com/RichiH/modbus_exporter/config"
	"github.com/RichiH/modbus_exporter/modbus"
)

// ModbusRequestStatusType possible status of the modbus request
type ModbusRequestStatusType string

const (
	// ModbusRequestStatusOK successful
	ModbusRequestStatusOK ModbusRequestStatusType = "OK"
	// ModbusRequestStatusErrorSock error opening socket connection
	ModbusRequestStatusErrorSock ModbusRequestStatusType = "ERROR_SOCKET"
	// ModbusRequestStatusErrorTimeout connection established but no response from modbus device
	ModbusRequestStatusErrorTimeout ModbusRequestStatusType = "ERROR_TIMEOUT"
	// ModbusRequestStatusErrorParsingValue error parsing value received
	ModbusRequestStatusErrorParsingValue ModbusRequestStatusType = "ERROR_PARSING_VALUE"
)

type SerialMutexStruct struct {
	mutexMap map[string]*sync.Mutex
	mutex    *sync.Mutex
}

func NewSerialMutexStruct() *SerialMutexStruct {
	return &SerialMutexStruct{mutexMap: make(map[string]*sync.Mutex), mutex: new(sync.Mutex)}
}

var mutex = NewSerialMutexStruct()

var (
	modbusDurationCounterVec            *prometheus.CounterVec
	modbusRequestsCounterVec            *prometheus.CounterVec
	modbusSerialMutexDurationCounterVec *prometheus.CounterVec
	modbusSerialMutexWaitersGaugeVec    *prometheus.GaugeVec
	modbusSerialRetriesCounterVec       *prometheus.CounterVec
)

func main() {
	var (
		configFile = kingpin.Flag(
			"config.file",
			"Sets the configuration file.",
		).Default("modbus.yml").String()
		toolkitFlags = webflag.AddFlags(kingpin.CommandLine, ":9602")
	)

	promlogConfig := &promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, promlogConfig)
	kingpin.Version(version.Print("modbus_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	logger := promlog.New(promlogConfig)

	level.Info(logger).Log("msg", "Starting modbus_exporter", "version", version.Info())
	level.Info(logger).Log("build_context", version.BuildContext())

	telemetryRegistry := prometheus.NewRegistry()
	telemetryRegistry.MustRegister(prometheus.NewGoCollector())
	telemetryRegistry.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	modbusDurationCounterVec = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "modbus_request_duration_seconds_total",
		Help: "Total duration of modbus successful requests by target in seconds",
	}, []string{"target", "modbus_target"})
	telemetryRegistry.MustRegister(modbusDurationCounterVec)

	modbusSerialMutexDurationCounterVec = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "modbus_request_serial_mutex_duration_seconds_total",
		Help: "Total duration of waiting for mutex lock for serial bus by serial bus and modbus_target in seconds",
	}, []string{"target", "modbus_target"})
	telemetryRegistry.MustRegister(modbusSerialMutexDurationCounterVec)

	modbusSerialMutexWaitersGaugeVec = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "modbus_request_serial_mutex_waiters",
		Help: "Total number of threads currently waiting for mutex lock by serial bus and modbus_target",
	}, []string{"target", "modbus_target"})
	telemetryRegistry.MustRegister(modbusSerialMutexWaitersGaugeVec)

	modbusSerialRetriesCounterVec = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "modbus_request_serial_retries_total",
		Help: "Total number of serial retries following errors by serial bus and modbus_target",
	}, []string{"target", "modbus_target"})
	telemetryRegistry.MustRegister(modbusSerialRetriesCounterVec)

	modbusRequestsCounterVec = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "modbus_requests_total",
		Help: "Number of modbus request by status and target",
	}, []string{"target", "modbus_target", "status"})
	telemetryRegistry.MustRegister(modbusRequestsCounterVec)

	level.Info(logger).Log("msg", "Loading configuration file", "config_file", *configFile)
	config, err := config.LoadConfig(*configFile)
	if err != nil {
		level.Error(logger).Log("msg", "Error loading config", "err", err)
		os.Exit(1)
	}

	http.Handle("/metrics", promhttp.HandlerFor(telemetryRegistry, promhttp.HandlerOpts{}))

	exporter := modbus.NewExporter(config)
	http.Handle("/modbus",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scrapeHandler(exporter, w, r, logger)
		}),
	)

	srv := &http.Server{}
	if err := web.ListenAndServe(srv, toolkitFlags, logger); err != nil {
		level.Error(logger).Log("msg", "Error starting HTTP server", "err", err)
		os.Exit(1)
	}
}

func scrapeHandler(e *modbus.Exporter, w http.ResponseWriter, r *http.Request, logger log.Logger) {
	moduleName := r.URL.Query().Get("module")
	if moduleName == "" {
		http.Error(w, "'module' parameter must be specified", http.StatusBadRequest)
		return
	}

	module := e.GetConfig().GetModule(moduleName)
	if module == nil {
		http.Error(w, fmt.Sprintf("module '%v' not defined in configuration file", moduleName), http.StatusBadRequest)
		return
	}

	target := r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, "'target' parameter must be specified", http.StatusBadRequest)
		return
	}

	sT := r.URL.Query().Get("sub_target")
	if sT == "" {
		http.Error(w, "'sub_target' parameter must be specified", http.StatusBadRequest)
		return
	}

	subTarget, err := strconv.ParseUint(sT, 10, 32)
	if err != nil {
		http.Error(w, fmt.Sprintf("'sub_target' parameter must be a valid integer: %v", err), http.StatusBadRequest)
		return
	}
	if subTarget > 255 {
		http.Error(w, fmt.Sprintf("'sub_target' parameter must be from 0 to 255. Invalid value: %d", subTarget), http.StatusBadRequest)
		return
	}

	level.Info(logger).Log("msg", "got scrape request", "module", moduleName, "target", target, "sub_target", subTarget)

	start := time.Now()
	if module.Protocol == config.ModbusProtocolSerial {
		modbusSerialMutexWaitersGaugeVec.WithLabelValues(target, fmt.Sprint(subTarget)).Inc()
		_, found := mutex.mutexMap[target]
		if !found {
			level.Info(logger).Log("msg", "creating target in mutexmap", "module", moduleName, "target", target, "subTarget", subTarget)
			mutex.mutex.Lock()
			mutex.mutexMap[target] = &sync.Mutex{}
			mutex.mutex.Unlock()
		}
		level.Info(logger).Log("prescrape locking inner mutex", "module", moduleName, "target", target, "subTarget", subTarget)
		mutex.mutexMap[target].Lock()
		modbusSerialMutexWaitersGaugeVec.WithLabelValues(target, fmt.Sprint(subTarget)).Dec()
		modbusSerialMutexDurationCounterVec.WithLabelValues(target, fmt.Sprint(subTarget)).Add(time.Since(start).Seconds())
	}
	gatherer, err := e.Scrape(target, byte(subTarget), moduleName)
	if module.Protocol == config.ModbusProtocolSerial {
		// retry up to two times when a serial scrape fails
		if err != nil {
			modbusSerialRetriesCounterVec.WithLabelValues(target, fmt.Sprint(subTarget)).Inc()
			gatherer, err = e.Scrape(target, byte(subTarget), moduleName)
		}
		if err != nil {
			modbusSerialRetriesCounterVec.WithLabelValues(target, fmt.Sprint(subTarget)).Inc()
			gatherer, err = e.Scrape(target, byte(subTarget), moduleName)
		}
		level.Info(logger).Log("postscrape unlocking inner mutex", "module", moduleName, "target", target, "subTarget", subTarget)
		mutex.mutexMap[target].Unlock()
	}
	duration := time.Since(start).Seconds()
	if err != nil {
		httpStatus := http.StatusInternalServerError
		if strings.Contains(fmt.Sprintf("%v", err), "unable to connect with target") {
			modbusRequestsCounterVec.WithLabelValues(target, fmt.Sprint(subTarget), string(ModbusRequestStatusErrorSock)).Inc()
			httpStatus = http.StatusServiceUnavailable
		} else if strings.Contains(fmt.Sprintf("%v", err), "i/o timeout") {
			modbusRequestsCounterVec.WithLabelValues(target, fmt.Sprint(subTarget), string(ModbusRequestStatusErrorTimeout)).Inc()
			httpStatus = http.StatusGatewayTimeout
		} else {
			modbusRequestsCounterVec.WithLabelValues(target, fmt.Sprint(subTarget), string(ModbusRequestStatusErrorParsingValue)).Inc()
		}
		http.Error(
			w,
			fmt.Sprintf("failed to scrape target '%v' sub_target '%d' with module '%v': %v", target, subTarget, moduleName, err),
			httpStatus,
		)
		level.Error(logger).Log("msg", "failed to scrape", "target", target, "module", moduleName, "err", err)
		return
	}
	modbusDurationCounterVec.WithLabelValues(target, fmt.Sprint(subTarget)).Add(duration)
	modbusRequestsCounterVec.WithLabelValues(target, fmt.Sprint(subTarget), string(ModbusRequestStatusOK)).Inc()

	promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}).ServeHTTP(w, r)
}
