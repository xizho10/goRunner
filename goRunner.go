package main

//author: Doug Watson

//macOS will need more open files than the default 256 if you want to run over a couple hundred goroutines
//launchctl limit maxfiles 10200

//For a really big test, if you need a million open files on a mac:
//nvram boot-args="serverperfmode=1"
//shutdown -r now
//launchctl limit maxfiles 999999
//ulimit -n 999999

import (
	"bufio"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"syscall"
	"time"
)

// -------------------------------------------------------------------------------------------------
// Flags
var (
	clients        int
	secsTimedTest  int
	debug          bool
	dump           bool
	connectTimeout int
	writeTimeout   int
	readTimeout    int
	targetTPS      float64
	baseUrl        string
	configFile     string
	inputFile      string
	inputFile1     string
	delimeter      string
	headerExit     bool
	noHeader       bool
	rampUp         string
	cpuProfile     string
	verbose        bool
	keepAlive      bool
)

func init() {
	flag.IntVar(&clients, "c", 100, "Number of concurrent clients to launch.")
	flag.IntVar(&secsTimedTest, "t", 0, "Time in seconds for a timed load test")
	flag.StringVar(&inputFile, "f", "", "Read input from file rather than stdin")
	flag.StringVar(&inputFile1, "fh", "", "Read input from csv file which has a header row")
	flag.StringVar(&rampUp, "rampUp", "0", "Specify ramp up delay as duration (1m2s, 300ms, ..). 'auto' will compute from client sessions. Default to 0 (no ramp up)")
	flag.Float64Var(&targetTPS, "targetTPS", 1000000, "The default max TPS is set to 1 million. Good luck reaching this :p")
	flag.StringVar(&baseUrl, "baseUrl", "", "The host to test. Example https://test2.someserver.org")
	flag.BoolVar(&debug, "debug", false, "Show the body returned for the api call")
	flag.BoolVar(&dump, "dump", false, "Show the HTTP dump for the api call")
	flag.StringVar(&configFile, "configFile", "config.ini", "Config file location")
	flag.StringVar(&delimeter, "d", ",", "Output file delimeter")
	flag.BoolVar(&headerExit, "hx", false, "Print output header row and exit")
	flag.BoolVar(&noHeader, "nh", false, "Suppress output header row. Ignored if hx is set")
	flag.IntVar(&readTimeout, "readtimeout", 30, "Timeout in seconds for the target API to send the first response byte. Default 30 seconds")
	flag.StringVar(&cpuProfile, "cpuprofile", "", "write cpu profile to file")
	flag.BoolVar(&verbose, "verbose", false, "verbose debugging output flag")
	flag.BoolVar(&keepAlive, "keepalive", true, "enable/disable keepalive")
}

// -------------------------------------------------------------------------------------------------
// Build commit id from git
var Build string

func init() {
	if Build == "" {
		Build = "unset"
	}
}

/*
func generateInput() chan string {
	lines := make(chan string)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		//scanner.Err()
		fmt.Printf("scanner done:\n\n")
	}()
	return lines
}
*/
func main() {
	// ---------------------------------------------------------------------------------------------
	// Parse flags
	flag.Parse()

	// ---------------------------------------------------------------------------------------------
	// Validate input
	if clients < 1 {
		fmt.Fprintf(os.Stderr, "\nNumber of concurrent client should be at least 1\n\n")
		flag.Usage()
		os.Exit(1)
	}

	if cpuProfile != "" {
		f, err := os.Create(cpuProfile)
		if err != nil {
			fmt.Println(err)
			return
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	Delimeter = delimeter //set the delimeter we want for the output log in the imported runner package
	if !headerExit && baseUrl == "" {
		fmt.Fprintf(os.Stderr, "\nPlease provide the baseUrl...\n\n")
		flag.Usage()
		os.Exit(1)
	}
	trafficChannel := make(chan string)
	//	startTraffic(trafficChannel) //start reading on the channel

	if verbose {
		println("verbose")
	}

	overallStartTime := time.Now()
	baseUrlFilter := regexp.MustCompile(baseUrl)
	configuration := NewConfiguration2(configFile)
	results := make(map[int]*Result)
	signalChannel := make(chan os.Signal, 2)
	signal.Notify(signalChannel, os.Interrupt, syscall.SIGTERM)
	go func() {
		_ = <-signalChannel
		ExitWithStatus(results, overallStartTime)
	}()
	goMaxProcs := os.Getenv("GOMAXPROCS")
	if goMaxProcs == "" {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	rampUpDelay := time.Duration(0)
	if !headerExit {
		printSessionSummary(configuration, configFile)
		if rampUp == "auto" {
			rampUpDelay = calcRampUpDelay(configuration)
		} else {
			var err error
			rampUpDelay, err = time.ParseDuration(rampUp)
			if err != nil {
				fmt.Println("invalid rampUp duration" + err.Error())
				os.Exit(1)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "Spacing sessions %v apart to ramp up to %d client sessions\n", rampUpDelay, clients)

	transportConfig := &tls.Config{InsecureSkipVerify: true} //allow wrong ssl certs
	tr := &http.Transport{TLSClientConfig: transportConfig}
	tr.ResponseHeaderTimeout = time.Second * time.Duration(readTimeout)
	var done sync.WaitGroup
	var stopTime time.Time // client loops will work to the end of trafficChannel unless we explicitly init a stopTime
	if secsTimedTest > 0 {
		stopTime = time.Now().Add(time.Duration(secsTimedTest) * time.Second)
	}
	for i := 0; i < clients; i++ {
		done.Add(1)
		result := &Result{}
		results[i] = result
		clientDelay := rampUpDelay * time.Duration(i)
		go client(tr, configuration, result, &done, trafficChannel, i, baseUrlFilter, stopTime, clientDelay)
	}

	if len(inputFile1) > 0 {
		if len(inputFile) > 0 {
			fmt.Fprintf(os.Stderr, "don't use -f and -fh together\n")
			os.Exit(1)
		} else {
			inputFile = inputFile1
		}
	}

	scanner := bufio.NewScanner(os.Stdin)
	if len(inputFile) > 0 {
		file, err := os.Open(inputFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err.Error())
			os.Exit(1)
		}
		defer file.Close()
		scanner = bufio.NewScanner(file)
	}

	_ = scanner.Scan()
	if !noHeader {
		PrintLogHeader(scanner.Text(), len(inputFile1) > 0)
		if headerExit {
			os.Exit(0)
		}
	}
	if len(inputFile1) > 0 && HasInputColHeaders() == false {
		// name our column headers
		HeadInputColumns(scanner.Text())
	} else {
		// anonymous column headers, put work into the channel instead
		trafficChannel <- scanner.Text()
	}

	for scanner.Scan() {
		trafficChannel <- scanner.Text() //put work into the channel from Stdin
	}
	close(trafficChannel)
	done.Wait()
	ExitWithStatus(results, overallStartTime)
}

func calcRampUpDelay(cfg *CfgStruct) time.Duration {
	if clients == 0 {
		return time.Duration(0)
	} else {
		dur := EstimateSessionTime(cfg)
		return dur / time.Duration(clients)
	}
}

//one client per -c=thread
//func noRedirect(req *http.Request, via []*http.Request) error {
//	return errors.New("Don't redirect!")
//}
func client(tr *http.Transport, configuration *CfgStruct, result *Result, done *sync.WaitGroup, trafficChannel chan string, clientId int, baseUrlFilter *regexp.Regexp, stopTime time.Time, initialDelay time.Duration) {
	defer done.Done()
	// strangely, this sleep cannot be moved outside the client function
	// or all client threads will wait until the last one is spawned before starting their own execution loops
	time.Sleep(initialDelay)

	msDelay := 0
	for id := range trafficChannel {
		//read in the line. either a generated id or (TODO) a url
		//		id := <-trafficChannel //urlTemplate : 100|10000|about

		//Do Heartbeat
		// cookieMap and sessionVars should start fresh every time we start a DoReq session
		var cookieMap = make(map[string]*http.Cookie)
		var sessionVars = make(map[string]string)
		DoReq(0, id, configuration, result, clientId, baseUrl, baseUrlFilter, msDelay, tr, cookieMap, sessionVars, stopTime, 0.0) //val,resp, err
		msDelay = PostSessionDelay
	}
	if time.Now().Before(stopTime) {
		fmt.Fprintf(os.Stderr, "client %d ran out of test input %.2fs before full test time\n", clientId, stopTime.Sub(time.Now()).Seconds())
	}
}

func printSessionSummary(configuration *CfgStruct, configFile string) {
	fmt.Fprintf(os.Stderr, "GORUNNER\n")
	if len(configuration.Version.ConfigVersion) > 5 {
		fmt.Fprintf(os.Stderr, "Configuration:                  %s version %s\n", configFile, configuration.Version.ConfigVersion[5:len(configuration.Version.ConfigVersion)-1])
	} else {
		fmt.Fprintf(os.Stderr, "Configuration:                  %s\n", configFile)
	}
	fmt.Fprintf(os.Stderr, "Session profile:                %d requests: %s\n", len(CommandQueue), strings.Join(CommandQueue, ", "))
	fmt.Fprintf(os.Stderr, "Estimated session time:         %v\n", EstimateSessionTime(configuration))
	fmt.Fprintf(os.Stderr, "Simultaneous sessions:          %d\n", clients)
	fmt.Fprintf(os.Stderr, "API host:                       %s\n", baseUrl)
	const layout = "2006-01-02 15:04:05"
	fmt.Fprintf(os.Stderr, "Test start time:                %v\n", time.Now().Format(layout))
}
