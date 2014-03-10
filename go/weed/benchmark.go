package main

import (
	"bufio"
	"code.google.com/p/weed-fs/go/glog"
	"code.google.com/p/weed-fs/go/operation"
	"code.google.com/p/weed-fs/go/util"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"time"
)

type BenchmarkOptions struct {
	server        *string
	concurrency   *int
	numberOfFiles *int
	fileSize      *int
	idListFile    *string
	write         *bool
	read          *bool
	vid2server    map[string]string //cache for vid locations
}

var (
	b BenchmarkOptions
)

func init() {
	cmdBenchmark.Run = runbenchmark // break init cycle
	cmdBenchmark.IsDebug = cmdBenchmark.Flag.Bool("debug", false, "verbose debug information")
	b.server = cmdBenchmark.Flag.String("server", "localhost:9333", "weedfs master location")
	b.concurrency = cmdBenchmark.Flag.Int("c", 7, "number of concurrent write or read processes")
	b.fileSize = cmdBenchmark.Flag.Int("size", 1024, "simulated file size in bytes")
	b.numberOfFiles = cmdBenchmark.Flag.Int("n", 1024*1024, "number of files to write for each thread")
	b.idListFile = cmdBenchmark.Flag.String("list", os.TempDir()+"/benchmark_list.txt", "list of uploaded file ids")
	b.write = cmdBenchmark.Flag.Bool("write", true, "enable write")
	b.read = cmdBenchmark.Flag.Bool("read", true, "enable read")
}

var cmdBenchmark = &Command{
	UsageLine: "benchmark -server=localhost:9333 -c=10 -n=100000",
	Short:     "benchmark on writing millions of files and read out",
	Long: `benchmark on an empty weed file system.
  
  Two tests during benchmark:
  1) write lots of small files to the system
  2) read the files out
  
  The file content is mostly zero, but no compression is done.
  
  By default, write 1 million files of 1KB each with 7 concurrent threads, 
  and randomly read them out with 7 concurrent threads.
  
  You can choose to only benchmark read or write.
  During write, the list of uploaded file ids is stored in "-list" specified file.
  You can also use your own list of file ids to run read test.
  
  Write speed and read speed will be collected.
  The numbers are used to get a sense of the system.
  But usually your network or the hard drive is 
  the real bottleneck.

  `,
}

var (
	wait       sync.WaitGroup
	writeStats *stats
	readStats  *stats
)

func runbenchmark(cmd *Command, args []string) bool {
	finishChan := make(chan bool)
	fileIdLineChan := make(chan string)
	b.vid2server = make(map[string]string)

	if *b.write {
		writeStats = newStats()
		idChan := make(chan int)
		wait.Add(*b.concurrency)
		go writeFileIds(*b.idListFile, fileIdLineChan, finishChan)
		for i := 0; i < *b.concurrency; i++ {
			go writeFiles(idChan, fileIdLineChan, writeStats)
		}
		writeStats.start = time.Now()
		for i := 0; i < *b.numberOfFiles; i++ {
			idChan <- i
		}
		close(idChan)
		wait.Wait()
		writeStats.end = time.Now()
		wait.Add(1)
		finishChan <- true
		wait.Wait()
		writeStats.printStats("Writing Benchmark")
	}

	if *b.read {
		readStats = newStats()
		wait.Add(*b.concurrency)
		go readFileIds(*b.idListFile, fileIdLineChan)
		readStats.start = time.Now()
		for i := 0; i < *b.concurrency; i++ {
			go readFiles(fileIdLineChan, readStats)
		}
		wait.Wait()
		readStats.end = time.Now()
		readStats.printStats("Randomly Reading Benchmark")
	}

	return true
}

func writeFiles(idChan chan int, fileIdLineChan chan string, s *stats) {
	for {
		if id, ok := <-idChan; ok {
			start := time.Now()
			fp := &operation.FilePart{Reader: &FakeReader{id: uint64(id), size: int64(*b.fileSize)}, FileSize: int64(*b.fileSize)}
			if assignResult, err := operation.Assign(*b.server, 1, ""); err == nil {
				fp.Server, fp.Fid = assignResult.PublicUrl, assignResult.Fid
				fp.Upload(0, *b.server, "")
				writeStats.addSample(time.Now().Sub(start))
				fileIdLineChan <- fp.Fid
				s.transferred += int64(*b.fileSize)
				s.completed++
				if *cmdBenchmark.IsDebug {
					fmt.Printf("writing %d file %s\n", id, fp.Fid)
				}
			} else {
				s.failed++
				println("writing file error:", err.Error())
			}
		} else {
			break
		}
	}
	wait.Done()
}

func readFiles(fileIdLineChan chan string, s *stats) {
	for {
		if fid, ok := <-fileIdLineChan; ok {
			if len(fid) == 0 {
				continue
			}
			if fid[0] == '#' {
				continue
			}
			if *cmdBenchmark.IsDebug {
				fmt.Printf("reading file %s\n", fid)
			}
			parts := strings.SplitN(fid, ",", 2)
			vid := parts[0]
			start := time.Now()
			if server, ok := b.vid2server[vid]; !ok {
				if ret, err := operation.Lookup(*b.server, vid); err == nil {
					if len(ret.Locations) > 0 {
						server = ret.Locations[0].PublicUrl
						b.vid2server[vid] = server
					}
				}
			}
			if server, ok := b.vid2server[vid]; ok {
				url := "http://" + server + "/" + fid
				if bytesRead, err := util.Get(url); err == nil {
					s.completed++
					s.transferred += int64(len(bytesRead))
					readStats.addSample(time.Now().Sub(start))
				} else {
					s.failed++
					println("!!!! Failed to read from ", url, " !!!!!")
				}
			} else {
				s.failed++
				println("!!!! volume id ", vid, " location not found!!!!!")
			}
		} else {
			break
		}
	}
	wait.Done()
}

func writeFileIds(fileName string, fileIdLineChan chan string, finishChan chan bool) {
	file, err := os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		glog.Fatalf("File to create file %s: %s\n", fileName, err)
	}
	defer file.Close()

	for {
		select {
		case <-finishChan:
			wait.Done()
			return
		case line := <-fileIdLineChan:
			file.Write([]byte(line))
			file.Write([]byte("\n"))
		}
	}
}

func readFileIds(fileName string, fileIdLineChan chan string) {
	file, err := os.Open(fileName) // For read access.
	if err != nil {
		glog.Fatalf("File to read file %s: %s\n", fileName, err)
	}
	defer file.Close()

	r := bufio.NewReader(file)
	for {
		if line, err := Readln(r); err == nil {
			fileIdLineChan <- string(line)
		} else {
			break
		}
	}
	close(fileIdLineChan)
}

const (
	benchResolution = 10000 //0.1 microsecond
	benchBucket     = 1000000000 / benchResolution
)

type stats struct {
	data        []int
	completed   int
	failed      int
	transferred int64
	start       time.Time
	end         time.Time
}

var percentages = []int{50, 66, 75, 80, 90, 95, 98, 99, 100}

func newStats() *stats {
	return &stats{data: make([]int, benchResolution)}
}

func (s stats) addSample(d time.Duration) {
	s.data[int(d/benchBucket)]++
}

func (s stats) printStats(testName string) {
	fmt.Printf("\n------------ %s ----------\n", testName)
	timeTaken := float64(int64(s.end.Sub(s.start))) / 1000000000
	fmt.Printf("Concurrency Level:      %d\n", *b.concurrency)
	fmt.Printf("Time taken for tests:   %.3f seconds\n", timeTaken)
	fmt.Printf("Complete requests:      %d\n", s.completed)
	fmt.Printf("Failed requests:        %d\n", s.failed)
	fmt.Printf("Total transferred:      %d bytes\n", s.transferred)
	fmt.Printf("Requests per second:    %.2f [#/sec]\n", float64(s.completed)/timeTaken)
	fmt.Printf("Transfer rate:          %.2f [Kbytes/sec]\n", float64(s.transferred)/1024/timeTaken)
	n, sum := 0, 0
	min, max := 10000000, 0
	for i := 0; i < len(s.data); i++ {
		n += s.data[i]
		sum += s.data[i] * i
		if s.data[i] > 0 {
			if min > i {
				min = i
			}
			if max < i {
				max = i
			}
		}
	}
	avg := float64(sum) / float64(n)
	varianceSum := 0.0
	for i := 0; i < len(s.data); i++ {
		if s.data[i] > 0 {
			d := float64(i) - avg
			varianceSum += d * d * float64(s.data[i])
		}
	}
	std := math.Sqrt(varianceSum / float64(n))
	fmt.Printf("\nConnection Times (ms)\n")
	fmt.Printf("              min      avg        max      std\n")
	fmt.Printf("Total:        %2.1f      %3.1f       %3.1f      %3.1f\n", float32(min)/10, float32(avg)/10, float32(max)/10, std/10)
	//printing percentiles
	fmt.Printf("\nPercentage of the requests served within a certain time (ms)\n")
	percentiles := make([]int, len(percentages))
	for i := 0; i < len(percentages); i++ {
		percentiles[i] = n * percentages[i] / 100
	}
	percentiles[len(percentiles)-1] = n
	percentileIndex := 0
	currentSum := 0
	for i := 0; i < len(s.data); i++ {
		currentSum += s.data[i]
		if s.data[i] > 0 && percentileIndex < len(percentiles) && currentSum >= percentiles[percentileIndex] {
			fmt.Printf("  %3d%%    %5.1f ms\n", percentages[percentileIndex], float32(i)/10.0)
			percentileIndex++
			for percentileIndex < len(percentiles) && currentSum >= percentiles[percentileIndex] {
				percentileIndex++
			}
		}
	}
}

// a fake reader to generate content to upload
type FakeReader struct {
	id   uint64 // an id number
	size int64  // max bytes
}

func (l *FakeReader) Read(p []byte) (n int, err error) {
	if l.size <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > l.size {
		n = int(l.size)
	} else {
		n = len(p)
	}
	for i := 0; i < n-8; i += 8 {
		for s := uint(0); s < 8; s++ {
			p[i] = byte(l.id >> (s * 8))
		}
	}
	l.size -= int64(n)
	return
}

func Readln(r *bufio.Reader) ([]byte, error) {
	var (
		isPrefix bool  = true
		err      error = nil
		line, ln []byte
	)
	for isPrefix && err == nil {
		line, isPrefix, err = r.ReadLine()
		ln = append(ln, line...)
	}
	return ln, err
}
