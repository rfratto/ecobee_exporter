package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rfratto/ecobee_exporter/ecobeeauth"
	"github.com/rspier/go-ecobee/ecobee"
	"golang.org/x/oauth2"
)

// flags
var (
	flagAPIKey       = flag.String("api-key", "", "ecobee API key")
	flagCacheFile    = flag.String("cache-file", "/tmp/ecobee-cache.json", "ecobee oauth cache")
	flagThermostatID = flag.String("thermostat-id", "", "ecobee thermostat ID to scrape")
	flagListenAddr   = flag.String("listen-addr", ":8080", "port to expose metrics on")
)

func main() {
	flag.Parse()
	if *flagAPIKey == "" {
		log.Fatalln("required flag unset: -api-key")
	} else if *flagThermostatID == "" {
		log.Fatalln("required flag unset: -thermostat-id")
	}

	ts, err := ecobeeauth.NewTokenSource(*flagAPIKey, *flagCacheFile)
	if err != nil {
		log.Fatalln(err)
	}
	cli := &ecobee.Client{Client: oauth2.NewClient(context.Background(), ts)}

	exporter := NewExporter(cli, *flagThermostatID)
	prometheus.MustRegister(exporter)

	r := mux.NewRouter()
	r.Handle("/metrics", promhttp.Handler())

	// /auth-start initates an pin code authorization
	r.HandleFunc("/auth-start", func(rw http.ResponseWriter, r *http.Request) {
		pr, err := ts.GetPin(r.Context())
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}

		if err := json.NewEncoder(rw).Encode(pr); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
	})

	// /auth-validate finishes a pin code authorization. An Authorization header
	// must be set with a Bearer token set to the value of "code" from the response
	// of the /auth-start flow. If the application hasn't been validated on Ecobee's
	// site yet, this call will fail.
	r.HandleFunc("/auth-validate", func(rw http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(rw, "not authorized", http.StatusUnauthorized)
			return
		}
		authHeader = strings.TrimPrefix(authHeader, "Bearer ")

		tok, err := ts.GetToken(r.Context(), authHeader)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}

		if err := ts.SaveToken(tok); err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		rw.WriteHeader(http.StatusOK)
	}).Methods(http.MethodPost)

	log.Println("listening on", *flagListenAddr)
	err = http.ListenAndServe(*flagListenAddr, r)
	if err != nil {
		log.Fatalln("failed to listen", err)
	}
}

func getThermostat(c *ecobee.Client, thermostatID string) (*ecobee.Thermostat, error) {
	s := ecobee.Selection{
		SelectionType:  "thermostats",
		SelectionMatch: thermostatID,

		IncludeAlerts:          false,
		IncludeEvents:          true,
		IncludeProgram:         true,
		IncludeRuntime:         true,
		IncludeExtendedRuntime: false,
		IncludeSettings:        false,
		IncludeSensors:         true,
		IncludeWeather:         true,
	}
	thermostats, err := c.GetThermostats(s)
	if err != nil {
		return nil, err
	} else if len(thermostats) != 1 {
		return nil, fmt.Errorf("got %d thermostats, wanted 1", len(thermostats))
	}
	return &thermostats[0], nil
}

func getThermostatSummary(c *ecobee.Client, thermostatID string) (*ecobee.ThermostatSummary, error) {
	tss, err := c.GetThermostatSummary(ecobee.Selection{
		SelectionType:  "thermostats",
		SelectionMatch: thermostatID,

		IncludeEquipmentStatus: true,
		IncludeAlerts:          false,
		IncludeEvents:          true,
		IncludeProgram:         true,
		IncludeRuntime:         true,
		IncludeExtendedRuntime: false,
		IncludeSettings:        false,
		IncludeSensors:         true,
		IncludeWeather:         true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed getting thermostat summary: %w", err)
	}

	summary, ok := tss[thermostatID]
	if !ok {
		return nil, fmt.Errorf("thermostat not found in summary")
	}
	return &summary, nil
}

type Exporter struct {
	cli          *ecobee.Client
	thermo       *ecobee.Thermostat
	summary      *ecobee.ThermostatSummary
	thermostatID string

	insideTemp  prometheus.Gauge
	outsideTemp prometheus.Gauge
	desiredHeat prometheus.Gauge
	desiredCool prometheus.Gauge
	cooling     *prometheus.GaugeVec
	heating     *prometheus.GaugeVec
	fanRunning  prometheus.Gauge
}

func NewExporter(cli *ecobee.Client, thermostatID string) *Exporter {
	return &Exporter{
		cli:          cli,
		thermostatID: thermostatID,

		insideTemp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ecobee_inside_temperature",
			Help: "Indoor temperature of the apartment.",
		}),
		outsideTemp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ecobee_outside_temperature",
			Help: "Outside temperature.",
		}),
		desiredHeat: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ecobee_desired_heat",
			Help: "Desired minimum temperature to heat to.",
		}),
		desiredCool: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ecobee_desired_cool",
			Help: "Desired maximum temperature to cool to.",
		}),
		cooling: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ecobee_cooling_stage",
			Help: "Stage of compressors for cooling that are running",
		}, []string{"stage"}),
		heating: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ecobee_heating_stage",
			Help: "Stage of pumps for heating that are running",
		}, []string{"stage"}),
		fanRunning: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ecobee_fan_running",
			Help: "1 if the fan is running",
		}),
	}
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	e.insideTemp.Describe(ch)
	e.outsideTemp.Describe(ch)
	e.desiredHeat.Describe(ch)
	e.desiredCool.Describe(ch)
	e.cooling.Describe(ch)
	e.heating.Describe(ch)
	e.fanRunning.Describe(ch)
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	if err := e.refreshThermo(); err != nil {
		log.Println("failed to refresh thermo", err)
		return
	}

	e.insideTemp.Set(float64(e.thermo.Runtime.ActualTemperature) / 10.0)
	e.desiredHeat.Set(float64(e.thermo.Runtime.DesiredHeat) / 10.0)
	e.desiredCool.Set(float64(e.thermo.Runtime.DesiredCool) / 10.0)

	if len(e.thermo.Weather.Forecasts) > 0 {
		temp := e.thermo.Weather.Forecasts[0].Temperature
		e.outsideTemp.Set(float64(temp) / 10.0)
	}

	e.cooling.WithLabelValues("CompCool1").Set(boolToFloat64(e.summary.CompCool1))
	e.cooling.WithLabelValues("CompCool2").Set(boolToFloat64(e.summary.CompCool2))

	e.heating.WithLabelValues("HeatPump").Set(boolToFloat64(e.summary.HeatPump))
	e.heating.WithLabelValues("HeatPump2").Set(boolToFloat64(e.summary.HeatPump2))
	e.heating.WithLabelValues("HeatPump3").Set(boolToFloat64(e.summary.HeatPump3))
	e.heating.WithLabelValues("AuxHeat1").Set(boolToFloat64(e.summary.AuxHeat1))
	e.heating.WithLabelValues("AuxHeat2").Set(boolToFloat64(e.summary.AuxHeat2))
	e.heating.WithLabelValues("AuxHeat3").Set(boolToFloat64(e.summary.AuxHeat3))

	e.fanRunning.Set(boolToFloat64(e.summary.Fan))

	e.insideTemp.Collect(ch)
	e.outsideTemp.Collect(ch)
	e.desiredHeat.Collect(ch)
	e.desiredCool.Collect(ch)
	e.cooling.Collect(ch)
	e.heating.Collect(ch)
	e.fanRunning.Collect(ch)
}

func (e *Exporter) refreshThermo() error {
	summary, err := getThermostatSummary(e.cli, e.thermostatID)
	if err != nil {
		return fmt.Errorf("failed refreshing thermo: %w", err)
	}
	e.summary = summary

	if e.thermo == nil || summary.RuntimeRevision != e.thermo.Runtime.RuntimeRev {
		log.Println("runtime revision changed, updating thermo object")

		t, err := getThermostat(e.cli, e.thermostatID)
		if err != nil {
			return fmt.Errorf("failed getting updated thermostat: %w", err)
		}

		e.thermo = t
	}

	return nil
}

func boolToFloat64(v bool) float64 {
	if v {
		return 1.0
	}
	return 0.0
}
