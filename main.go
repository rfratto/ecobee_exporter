package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rspier/go-ecobee/ecobee"
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

	cli := ecobee.NewClient(*flagAPIKey, *flagCacheFile)

	// Immediately collect the thermostat object for caching
	thermo, err := getThermostat(cli, *flagThermostatID)
	if err != nil {
		log.Fatalln("could not get thermostat at startup", err)
	}

	exporter := NewExporter(cli, thermo)

	prometheus.MustRegister(exporter)

	log.Println("listening on", *flagListenAddr)
	err = http.ListenAndServe(*flagListenAddr, promhttp.Handler())
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

type Exporter struct {
	cli    *ecobee.Client
	thermo *ecobee.Thermostat

	insideTemp  prometheus.Gauge
	outsideTemp prometheus.Gauge
	desiredHeat prometheus.Gauge
	desiredCool prometheus.Gauge
}

func NewExporter(cli *ecobee.Client, thermo *ecobee.Thermostat) *Exporter {
	return &Exporter{
		cli:    cli,
		thermo: thermo,

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
	}
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	e.insideTemp.Describe(ch)
	e.outsideTemp.Describe(ch)
	e.desiredHeat.Describe(ch)
	e.desiredCool.Describe(ch)
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

	e.insideTemp.Collect(ch)
	e.outsideTemp.Collect(ch)
	e.desiredHeat.Collect(ch)
	e.desiredCool.Collect(ch)
}

func (e *Exporter) refreshThermo() error {
	tss, err := e.cli.GetThermostatSummary(ecobee.Selection{
		SelectionType:  "thermostats",
		SelectionMatch: e.thermo.Identifier,

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
		return fmt.Errorf("failed getting thermostat summary: %w", err)
	}

	summary, ok := tss[e.thermo.Identifier]
	if !ok {
		return fmt.Errorf("thermostat not found in summary")
	}

	if summary.RuntimeRevision != e.thermo.Runtime.RuntimeRev {
		log.Println("runtime revision changed, updating thermo object")

		t, err := getThermostat(e.cli, e.thermo.Identifier)
		if err != nil {
			return fmt.Errorf("failed getting updated thermostat: %w", err)
		}

		e.thermo = t
	}

	return nil
}
