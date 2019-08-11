package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rspier/go-ecobee/ecobee"
)

// flags
var (
	flagAPIKey          = flag.String("api-key", "", "ecobee API key")
	flagCacheFile       = flag.String("cache-file", "/tmp/ecobee-cache.json", "ecobee oauth cache")
	flagThermostatID    = flag.String("thermostat-id", "", "ecobee thermostat ID to scrape")
	flagRefreshInterval = flag.Duration("refresh-interval", 5*time.Minute, "frequency to poll thermostat data. ecobee server only updates once per 15 minutes")
	flagListenAddr      = flag.String("listen-addr", ":8080", "port to expose metrics on")
)

// metrics
var (
	insideTemp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ecobee_inside_temperature",
		Help: "Indoor temperature of the apartment.",
	})
	outsideTemp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ecobee_outside_temperature",
		Help: "Outside temperature.",
	})
	desiredHeat = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ecobee_desired_heat",
		Help: "Desired minimum temperature to heat to.",
	})
	desiredCool = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ecobee_desired_cool",
		Help: "Desired maximum temperature to cool to.",
	})
)

func init() {
	prometheus.MustRegister(insideTemp)
	prometheus.MustRegister(outsideTemp)
	prometheus.MustRegister(desiredHeat)
	prometheus.MustRegister(desiredCool)
}

func main() {
	flag.Parse()
	if *flagAPIKey == "" {
		log.Fatalln("required flag unset: -api-key")
	} else if *flagThermostatID == "" {
		log.Fatalln("required flag unset: -thermostat-id")
	}

	cli := ecobee.NewClient(*flagAPIKey, *flagCacheFile)
	refreshData(cli, *flagThermostatID)

	go func() {
		c := time.Tick(*flagRefreshInterval)
		for range c {
			refreshData(cli, *flagThermostatID)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(*flagListenAddr, nil))
}

func refreshData(c *ecobee.Client, thermostatID string) {
	log.Println("refreshing thermostat data")
	t, err := getThermostat(c, thermostatID)
	if err != nil {
		panic(err)
	}

	insideTemp.Set(float64(t.Runtime.ActualTemperature) / 10.0)
	desiredHeat.Set(float64(t.Runtime.DesiredHeat) / 10.0)
	desiredCool.Set(float64(t.Runtime.DesiredCool) / 10.0)

	if len(t.Weather.Forecasts) > 0 {
		temp := t.Weather.Forecasts[0].Temperature
		outsideTemp.Set(float64(temp) / 10.0)
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
