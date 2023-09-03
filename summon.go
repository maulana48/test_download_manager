package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type downloader func(wg *sync.WaitGroup) error

type summon struct {
	concurrency      int64       // No. of connections
	uri              string      // URL of the file we want to download
	isResume         bool        // is this a resume request
	isRangeSupported bool        // if this request supports range
	err              error       // used when error occurs inside a goroutine
	startTime        time.Time   // to track time took
	fileDetails      fileDetails // will hold the file related details
	metaData         meta        // Will hold the meta data of the range and file details
	progressBar      progressBar // index => progress
	stop             chan error  // to handle stop signals from terminal
	separator        string      // store the path separator based on the OS
	*sync.RWMutex                // mutex to lock the maps which accessing it concurrently
}

type fileDetails struct {
	chunks        map[int64]*os.File // Map of part files we are creating
	fileName      string             // name of the file we are downloading
	fileDir       string             // dir of the file
	absolutePath  string             // absolute path of the output file
	tempOutFile   *os.File           // output / downloaded file
	resume        map[int64]resume   // how much is downloaded
	contentLength int64
}

func NewSummon() (*summon, error) {
	args := arguments{}

	parseFlags(&args)

	if args.help {
		flag.PrintDefaults()
		fmt.Println("\nExample Usage - $GOBIN/summon -c 5 http://www.africau.edu/images/default/sample.pdf")
		os.Exit(0)
	}

	// Set logger
	setLogWriter(args.verbose)

	sum := new(summon)

	fileURL, err := validate()
	if err != nil {
		return sum, err
	}

	sum.uri = fileURL
	sum.fileDetails.chunks = make(map[int64]*os.File)
	sum.startTime = time.Now()
	sum.fileDetails.fileName = filepath.Base(sum.uri)
	sum.RWMutex = &sync.RWMutex{}
	sum.progressBar.RWMutex = &sync.RWMutex{}
	sum.progressBar.p = make(map[int64]*progress)
	sum.stop = make(chan error)
	sum.separator = string(os.PathSeparator)
	sum.fileDetails.resume = make(map[int64]resume)

	sum.setConcurrency(args.connections)
	sum.setAbsolutePath(args.outputFile)
	sum.setFileDir()

	if err := sum.createTempOutputFile(); err != nil {
		return nil, err
	}

	return sum, nil
}

func validate() (string, error) {
	if len(flag.Args()) <= 0 {
		return "", fmt.Errorf("please pass file url")
	}

	u := flag.Args()[0]
	uri, err := url.ParseRequestURI(u)
	if err != nil {
		return "", fmt.Errorf("passed URL is invalid")
	}

	return uri.String(), nil
}

// process is the manager method
func (sum *summon) process() error {
	progressSize = getProgressSize()

	// pwg is the progressbar waitgroup
	wg, pWg := &sync.WaitGroup{}, &sync.WaitGroup{}

	if err := sum.getDownloader()(wg); err != nil {
		return err
	}

	stop := make(chan struct{})

	pWg.Add(1)
	// Keep Printing Progress
	go sum.startProgressBar(pWg, stop)
	wg.Wait()

	// Defer file closing
	defer sum.fileDetails.tempOutFile.Close()
	for _, f := range sum.fileDetails.chunks {
		defer f.Close()
	}

	stop <- struct{}{}

	// Wait for progressbar function to stop
	pWg.Wait()

	if sum.err != nil {
		return sum.err
	}

	return sum.combineChunks()
}

// The reason for this type is that our download & resumeDownload have the same method definition.  We have also created  getDownloader method which returns a downloader.
func (sum *summon) getDownloader() downloader {
	if sum.isResume {
		return sum.resumeDownload
	}

	return sum.download
}

func (sum summon) getTempFileName(index, start, end int64) (string, error) {
	return fmt.Sprintf("%s%s.%s.sump%d", sum.fileDetails.fileDir, sum.separator, sum.fileDetails.fileName, index), nil
}

// setConcurrency set the concurrency as per min and max
func (sum *summon) setConcurrency(c int64) {
	// We use default connections in case no concurrency is passed
	if c <= 0 {
		log.Println("Using default number of connections", DEFAULT_CONN)
		sum.concurrency = DEFAULT_CONN
		return
	}

	if c >= MAX_CONN {
		sum.concurrency = MAX_CONN
		return
	}

	sum.concurrency = c
}

func (sum *summon) setAbsolutePath(opath string) error {
	if opath == "" {
		filename, err := getFileNameFromHeaders(sum.uri)
		if err != nil {
			return err
		}

		if filename == "" {
			// Get the filename from the url
			opath = filepath.Base(sum.uri)
		} else {
			LogWriter.Printf("Got Filename from headers : %v", filename)
			sum.fileDetails.fileName = filename
			opath = filename
		}

	}

	if filepath.IsAbs(opath) {
		LogWriter.Printf("path passed is an absolute path")
		sum.fileDetails.absolutePath = opath
		return nil
	}

	absPath, err := filepath.Abs(opath)
	if err != nil {
		LogWriter.Printf("error while getting absolute path : %v", err)
		return err
	}
	LogWriter.Printf("Final absolute path is : %v", absPath)

	sum.fileDetails.absolutePath = absPath

	return nil
}

func (sum *summon) setFileDir() {
	sum.fileDetails.fileDir = filepath.Dir(sum.fileDetails.absolutePath)
}

// combineChunks will combine the chunks in ordered fashion starting from 1
func (sum *summon) combineChunks() error {
	LogWriter.Printf("Combining the files...")

	var w int64
	// maps are not ordered hence using for loop
	for i := int64(0); i < int64(len(sum.fileDetails.chunks)); i++ {
		handle := sum.fileDetails.chunks[i]

		if handle == nil {
			return fmt.Errorf("got chunk handle nil")
		}

		handle.Seek(0, 0) // We need to seek because read and write cursor are same and the cursor would be at the end.
		written, err := io.Copy(sum.fileDetails.tempOutFile, handle)
		if err != nil {
			return fmt.Errorf("error occured while copying to temp file : %v", err)
		}
		w += written
	}

	tempFileName := sum.fileDetails.tempOutFile.Name()

	finalFileName := sum.fileDetails.fileDir + sum.separator + sum.fileDetails.fileName

	log.Printf("Wrote to File : %v, Written : %v", finalFileName, humanSizeFromBytes(w))

	LogWriter.Printf("Renaming File from : %v to %v", tempFileName, finalFileName)

	if err := os.Rename(tempFileName, finalFileName); err != nil {
		return fmt.Errorf("error occured while renaming file : %v", err)
	}

	return nil
}

// downloadFileForRange will download the file for the provided range and set the bytes to the chunk map, will set summor.error field if error occurs
func (sum *summon) downloadFileForRange(wg *sync.WaitGroup, r string, index int64, handle io.Writer) {
	LogWriter.Printf("Downloading for range : %s , for index : %d", r, index)
	defer wg.Done()

	request, err := http.NewRequest("GET", sum.uri, strings.NewReader(""))
	if err != nil {
		sum.Lock()
		sum.err = err
		sum.Unlock()
		return
	}

	request.Header.Add("Range", "bytes="+r)

	client := http.Client{Timeout: 0}

	response, err := client.Do(request)
	if err != nil {
		sum.Lock()
		sum.err = err
		sum.Unlock()
		return
	}

	// 206 = Partial Content
	if response.StatusCode != 200 && response.StatusCode != 206 {
		sum.Lock()
		sum.err = fmt.Errorf("did not get 20X status code, got : %v", response.StatusCode)
		sum.Unlock()
		log.Println(sum.err)
		return
	}

	if err := sum.getDataAndWriteToFile(response.Body, handle, index); err != nil {
		sum.Lock()
		sum.err = err
		sum.Unlock()
		log.Println(sum.err)
		return
	}
}

// getRangeDetails returns ifRangeIsSupported,statuscode,error
func getRangeDetails(u string) (bool, int64, error) {
	request, err := http.NewRequest("HEAD", u, strings.NewReader(""))
	if err != nil {
		return false, 0, fmt.Errorf("error while creating request : %v", err)
	}

	sc, headers, _, err := doAPICall(request)
	if err != nil {
		return false, 0, fmt.Errorf("error calling url : %v", err)
	}

	if sc != 200 && sc != 206 {
		return false, 0, fmt.Errorf("did not get 200 or 206 response")
	}

	conLen := headers.Get("Content-Length")

	cl, err := parseint64(conLen)
	if err != nil {
		return false, 0, fmt.Errorf("error Parsing content length : %v", err)
	}

	// Accept-Ranges: bytes
	if headers.Get("Accept-Ranges") == "bytes" {
		return true, cl[0], nil
	}

	return false, cl[0], nil
}

// doAPICall will do the api call and return statuscode,headers,data,error respectively
func doAPICall(request *http.Request) (int, http.Header, []byte, error) {
	client := http.Client{
		Timeout: 5 * time.Second,
	}

	response, err := client.Do(request)
	if err != nil {
		return 0, http.Header{}, []byte{}, fmt.Errorf("error while doing request : %v", err)
	}
	defer response.Body.Close()

	data, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, http.Header{}, []byte{}, fmt.Errorf("error while reading response body : %v", err)
	}

	return response.StatusCode, response.Header, data, nil
}

func getFileNameFromHeaders(u string) (string, error) {
	request, err := http.NewRequest("HEAD", u, strings.NewReader(""))
	if err != nil {
		return "", err
	}

	sc, headers, _, err := doAPICall(request)
	if err != nil {
		return "", err
	}

	if sc != 200 {
		return "", fmt.Errorf("did not get 200 response in getFileNameFromHeaders : %v", sc)
	}

	cd := headers.Get("Content-Disposition")

	// Content-Disposition is not present so filename is not there
	if cd == "" {
		LogWriter.Printf("getFileNameFromHeaders got content disposation empty")
		return "", nil
	}

	_, params, err := mime.ParseMediaType(cd)
	LogWriter.Printf("params : %v", params)
	if err != nil {
		return "", err
	}

	return params["filename"], nil
}

// getDataAndWriteToFile will get the response and write to file
func (sum *summon) getDataAndWriteToFile(body io.ReadCloser, f io.Writer, index int64) error {
	defer body.Close()

	// we make buffer of 500 bytes and try to read 500 bytes every iteration.
	var buf = make([]byte, 500)

	defer startTimer("Time took for chunk : %v is", index)()

	for {
		select {
		case <-sum.stop:
			return ErrGracefulShutdown
		default:
			err := sum.readBody(body, f, buf, index)
			if err == io.EOF {
				return nil
			}

			if err != nil {
				return err
			}
		}
	}
}

func (sum *summon) readBody(body io.Reader, f io.Writer, buf []byte, index int64) error {
	r, err := body.Read(buf)

	if r > 0 {
		f.Write(buf[:r])

		sum.progressBar.Lock()
		sum.progressBar.p[index].curr += int64(r)
		sum.progressBar.Unlock()
	}

	if err != nil {
		return err
	}

	return nil
}
