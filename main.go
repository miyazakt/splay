package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"golang.org/x/xerrors"
	yaml "gopkg.in/yaml.v2"
)

/*
scenarios:
  - name: ping
    url: https://google.com
    throughput: 1
    validates:
    - name: status_code=200
      status_code: 200
*/

var (
	httpWorkerNum = 100
	httpTimeout   = 10
)

func init() {
	http.DefaultTransport.(*http.Transport).MaxIdleConns = 0
	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = 3000
}

// ScenarioData is scenario yaml file structure
type ScenarioData struct {
	Scenarios []Scenario `yaml:",flow"`
}

// Scenario is scenario data
type Scenario struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`

	Period     *int    `yaml:"period"`
	Count      *int    `yaml:"count"`
	Throughput float64 `yaml:"throughput"`

	Validates []Validate `yaml:",flow"`
}

// Validate is scenario validation structure
type Validate struct {
	Name string `yaml:"name"`

	StatusCode *int `yaml:"status_code"`
}

// ResultState is state of scenario result
type ResultState int

// Result enum
const (
	ResultOK ResultState = iota
	ResultValidationFail
	ResultRequestFail
)

// LoadScenarioFile read file and map ScenarioData
func LoadScenarioFile(in io.Reader) (*ScenarioData, error) {
	bytes, err := ioutil.ReadAll(in)
	if err != nil {
		return nil, err
	}

	s := ScenarioData{}

	if err := yaml.Unmarshal(bytes, &s); err != nil {
		return nil, err
	}

	return &s, nil
}

func scenarioWorker(
	ctx context.Context,
	scenarioCh <-chan Scenario,
	done chan<- struct{},
	reportCh chan<- ResultState) {
L:
	for {
		var s Scenario
		select {
		case <-ctx.Done():
			return
		case s = <-scenarioCh:
		}
		req, err := http.NewRequest("GET", s.URL, nil)
		if err != nil {
			log.Printf("[%s] Error: %s", s.Name, err)
			reportCh <- ResultRequestFail
			done <- struct{}{}
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(httpTimeout)*time.Second)
		req = req.WithContext(ctx)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("[%s] Error: %s", s.Name, err)
			reportCh <- ResultRequestFail
			cancel()
			done <- struct{}{}
			return
		}
		_ = resp.Body.Close()

		for _, v := range s.Validates {
			// validate
			if v.StatusCode != nil && resp.StatusCode != *v.StatusCode {
				err := xerrors.Errorf("%s: status code is invalid: expected: %v, got: %v", v.Name, *v.StatusCode, resp.StatusCode)
				reportCh <- ResultValidationFail
				log.Printf("[%s] Error: %s", s.Name, err)
				cancel()
				continue L
			}
		}
		log.Printf("[%s] Success", s.Name)
		reportCh <- ResultOK
		cancel()
		done <- struct{}{}
	}
}

// ScenarioReport is aggregated scenario result
type ScenarioReport struct {
	SuccessCount        int
	ValidationFailCount int
	RequestFailCount    int
}

// ScenarioRun runs scenario with context
func ScenarioRun(ctx context.Context, s Scenario) ScenarioReport {
	rl := rate.NewLimiter(rate.Limit(s.Throughput), 1)

	rlCh := make(chan struct{})
	go func() {
		defer close(rlCh)
		for {
			err := rl.Wait(ctx)
			if err != nil {
				return
			}
			rlCh <- struct{}{}
		}
	}()

	var count int
	if s.Period != nil {
		count = int(math.Ceil(float64(*s.Period) * s.Throughput))
	} else {
		count = *s.Count
	}

	done := make(chan struct{})
	scenarioCh := make(chan Scenario, httpWorkerNum)
	reportCh := make(chan ResultState)

	for i := 0; i < httpWorkerNum; i++ {
		go scenarioWorker(ctx, scenarioCh, done, reportCh)
	}

	go func() {
		defer close(reportCh)
		var c int
		for i := 1; i <= count; i++ {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-rlCh:
				if !ok {
					return
				}
				c++
				scenarioCh <- s
			}
		}

		for i := 0; i < c; i++ {
			<-done
		}
	}()

	var success, validationFail, requestFail int
	for state := range reportCh {
		switch state {
		case ResultOK:
			success++
		case ResultValidationFail:
			validationFail++
		case ResultRequestFail:
			requestFail++
		default:
		}
	}
	return ScenarioReport{
		SuccessCount:        success,
		ValidationFailCount: validationFail,
		RequestFailCount:    requestFail,
	}
}

func main() {
	scenarioFileName := flag.String("f", "scenario.yml", "scenario file")
	flag.IntVar(&httpWorkerNum, "c", 100, "http request concurrency per scenario")
	flag.Parse()

	f, err := os.Open(*scenarioFileName)
	if err != nil {
		log.Fatal(err)
	}

	scenario, err := LoadScenarioFile(f)
	_ = f.Close()
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		sig := <-c
		switch {
		case sig == os.Interrupt:
			fmt.Println("stop")
			cancel()
		default:
			log.Println("Unknown")
		}

	}()

	wg := sync.WaitGroup{}
	mutex := sync.Mutex{}
	reports := make(map[string]ScenarioReport)
	for _, s := range scenario.Scenarios {
		wg.Add(1)
		go func(s Scenario) {
			defer wg.Done()
			defer mutex.Unlock()

			report := ScenarioRun(ctx, s)
			mutex.Lock()
			reports[s.Name] = report
		}(s)
	}

	log.Println("Running")
	wg.Wait()
	log.Println("--------------------Result--------------------")

	for name, report := range reports {
		var (
			success        = report.SuccessCount
			validationFail = report.ValidationFailCount
			requestFail    = report.RequestFailCount
		)
		log.Printf("finished|[%s]\tsuccess: %d, validation fail: %d, request fail: %d",
			name, success, validationFail, requestFail)
	}
}
