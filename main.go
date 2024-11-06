package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/disintegration/imaging"
	"github.com/nfnt/resize"
	"github.com/spf13/cobra"
	"image"
	_ "image/png"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	inputPath    string
	outputPath   string
	compression  int
	maxWidth     int
	maxHeight    int
	outputFormat string
	configFile   string
	parallelism  int
)

const maxRetries = 1

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	logFile, err := os.OpenFile("processing.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("Failed to open log file: %v", err)
	}
	defer logFile.Close()
	log.SetOutput(io.MultiWriter(os.Stdout, logFile))

	var rootCmd = &cobra.Command{
		Use:   "thumbnailer",
		Short: "Thumbnailer creates thumbnails of images",
		Run:   run,
	}

	rootCmd.Flags().StringVarP(&inputPath, "input", "i", "", "Path to the input images")
	rootCmd.Flags().StringVarP(&outputPath, "output", "o", "", "Path to save the output thumbnails")
	rootCmd.Flags().IntVarP(&compression, "compression", "c", 75, "Compression level (1-100)")
	rootCmd.Flags().IntVarP(&maxWidth, "width", "w", 0, "Maximum width of the output thumbnails")
	rootCmd.Flags().IntVarP(&maxHeight, "height", "H", 0, "Maximum height of the output thumbnails")
	rootCmd.Flags().StringVarP(&outputFormat, "format", "f", "jpeg", "Output image format (jpeg, png)")
	rootCmd.Flags().StringVarP(&configFile, "config", "C", "", "Path to the configuration file")
	rootCmd.Flags().IntVarP(&parallelism, "parallelism", "p", runtime.NumCPU(), "Number of parallel image processing tasks")

	rootCmd.MarkFlagRequired("input")
	rootCmd.MarkFlagRequired("output")

	if err := rootCmd.Execute(); err != nil {
		log.Fatalf("Error executing command: %v", err)
	}
}

func run(cmd *cobra.Command, args []string) {
	if configFile != "" {
		if err := readConfig(configFile); err != nil {
			log.Fatalf("Error reading config file: %v", err)
		}
	}

	if maxWidth == 0 && maxHeight == 0 {
		log.Fatal("Either max width or max height must be specified")
	}

	// Ensure the output directory exists
	if err := os.MkdirAll(outputPath, os.ModePerm); err != nil {
		log.Fatalf("Error creating output directory: %v", err)
	}

	var files []string
	err := filepath.Walk(inputPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		log.Fatalf("Error reading input path: %v", err)
	}

	log.Printf("Starting processing of %d images", len(files))
	startTime := time.Now()

	var wg sync.WaitGroup
	sem := make(chan struct{}, parallelism)
	var successCount, errorCount int
	var mu sync.Mutex
	var durations []time.Duration

	for _, file := range files {
		wg.Add(1)
		sem <- struct{}{}

		go func(file string) {
			defer wg.Done()
			defer func() { <-sem }()

			retries := 0
			for retries < maxRetries {
				duration, err := processImage(file)
				if err != nil {
					log.Printf("Error processing image %s: %v", file, err)
					retries++
					if retries == maxRetries {
						mu.Lock()
						errorCount++
						mu.Unlock()
					}
				} else {
					mu.Lock()
					successCount++
					durations = append(durations, duration)
					mu.Unlock()
					break
				}
			}
		}(file)
	}

	wg.Wait()
	endTime := time.Now()
	log.Printf("Finished processing images in %v", endTime.Sub(startTime))
	log.Printf("Successfully processed %d images, encountered %d errors", successCount, errorCount)

	generateSummaryReport(len(files), successCount, errorCount, endTime.Sub(startTime), durations)
}

func generateSummaryReport(total, success, errors int, duration time.Duration, durations []time.Duration) {
	report := fmt.Sprintf("Summary Report:\n"+
		"Total images processed: %d\n"+
		"Successfully processed: %d\n"+
		"Errors encountered: %d\n"+
		"Total time taken: %v\n",
		total, success, errors, duration)

	for i, d := range durations {
		report += fmt.Sprintf("Image %d processing time: %v\n", i+1, d)
	}

	reportFile := filepath.Join(outputPath, "summary_report.txt")
	if err := ioutil.WriteFile(reportFile, []byte(report), 0644); err != nil {
		log.Fatalf("Error writing summary report: %v", err)
	}

	log.Printf("Summary report saved to %s", reportFile)
}

func readConfig(file string) error {
	data, err := ioutil.ReadFile(file)
	if err != nil {
		return err
	}

	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}

	if v, ok := config["input"].(string); ok {
		inputPath = v
	}
	if v, ok := config["output"].(string); ok {
		outputPath = v
	}
	if v, ok := config["compression"].(float64); ok {
		compression = int(v)
	}
	if v, ok := config["width"].(float64); ok {
		maxWidth = int(v)
	}
	if v, ok := config["height"].(float64); ok {
		maxHeight = int(v)
	}
	if v, ok := config["format"].(string); ok {
		outputFormat = v
	}

	return nil
}

func processImage(file string) (time.Duration, error) {
	log.Printf("Starting processing of image %s", file)
	startTime := time.Now()

	var img image.Image
	var err error

	if strings.HasSuffix(file, ".cr3") {
		// Convert CR3 to JPEG using exiftool
		jpegFile := strings.TrimSuffix(file, ".cr3") + ".jpg"
		cmd := exec.Command("exiftool", "-b", "-JpgFromRaw", "-w", "jpg", file)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err != nil {
			return 0, fmt.Errorf("error converting CR3 to JPEG: %v, %s", err, stderr.String())
		}
		file = jpegFile
	}

	imgFile, err := os.Open(file)
	if err != nil {
		return 0, fmt.Errorf("error opening image file %s: %v", file, err)
	}
	defer imgFile.Close()

	img, _, err = image.Decode(imgFile)
	if err != nil {
		return 0, fmt.Errorf("error decoding image file %s: %v", file, err)
	}

	if maxWidth > 0 && maxHeight > 0 {
		img = imaging.Fit(img, maxWidth, maxHeight, imaging.Lanczos)
	} else if maxWidth > 0 {
		img = resize.Resize(uint(maxWidth), 0, img, resize.Lanczos3)
	} else {
		img = resize.Resize(0, uint(maxHeight), img, resize.Lanczos3)
	}

	outputFile := filepath.Join(outputPath, strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))+"."+outputFormat)
	switch outputFormat {
	case "jpeg":
		err = imaging.Save(img, outputFile, imaging.JPEGQuality(compression))
	case "png":
		err = imaging.Save(img, outputFile)
	case "gif":
		err = imaging.Save(img, outputFile)
	case "bmp":
		err = imaging.Save(img, outputFile)
	default:
		return 0, fmt.Errorf("unsupported output format: %s", outputFormat)
	}

	if err != nil {
		return 0, fmt.Errorf("error saving image %s: %v", outputFile, err)
	}

	endTime := time.Now()
	duration := endTime.Sub(startTime)
	log.Printf("Finished processing image %s in %v", file, duration)

	return duration, nil
}

// Add this function to read RAW and Canon images
func readRawImage(file string) (image.Image, error) {
	data, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}
}
