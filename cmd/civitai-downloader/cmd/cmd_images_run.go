package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuri/uilive"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	index "go-civitai-download/index"
	"go-civitai-download/internal/downloader"
	"go-civitai-download/internal/models"
)

// runImages orchestrates the fetching and downloading of images based on command-line flags.
func runImages(cmd *cobra.Command, args []string) {
	// Read flags
	modelID := viper.GetInt("images.modelId")
	modelVersionID := viper.GetInt("images.modelVersionId")
	username := viper.GetString("images.username")
	limit := viper.GetInt("images.limit")
	period := viper.GetString("images.period")
	sort := viper.GetString("images.sort")
	nsfw := viper.GetString("images.nsfw")
	targetDir := viper.GetString("images.output_dir")
	saveMeta := viper.GetBool("images.metadata")
	numWorkers := viper.GetInt("images.concurrency")
	maxPages := viper.GetInt("images.max_pages")
	postID := viper.GetInt("images.postId")

	// --- Early Exit for Debug Print API URL --- START ---
	if printUrl, _ := cmd.Flags().GetBool("debug-print-api-url"); printUrl {
		log.Info("--- Debug API URL (--debug-print-api-url) for Images ---")
		// Construct URL parameters (logic duplicated/extracted from below)
		baseURL := "https://civitai.com/api/v1/images"
		params := url.Values{}
		if modelVersionID != 0 {
			params.Set("modelVersionId", strconv.Itoa(modelVersionID))
		} else if modelID != 0 {
			params.Set("modelId", strconv.Itoa(modelID))
		} else if username != "" {
			params.Set("username", username)
		} else if postID != 0 {
			params.Set("postId", strconv.Itoa(postID))
		}
		if limit > 0 && limit <= 200 {
			params.Set("limit", strconv.Itoa(limit))
		} else if limit != 100 {
			log.Warnf("Invalid limit %d, using API default (100). Actual API call might use different default.", limit)
			params.Set("limit", "100")
		}
		if period != "" {
			params.Set("period", period)
		}
		if sort != "" {
			params.Set("sort", sort)
		}
		if nsfw != "" {
			params.Set("nsfw", nsfw)
		}
		// Note: Does not include cursor logic, as this prints the base URL for the first page.
		requestURL := baseURL + "?" + params.Encode()
		fmt.Println(requestURL) // Print only the URL to stdout
		log.Info("Exiting after printing images API URL.")
		os.Exit(0) // Exit immediately
	}
	// --- Early Exit for Debug Print API URL --- END ---

	// --- Display Effective Config & Confirm --- START ---
	// Skip display/confirmation if global --yes flag is provided
	if !viper.GetBool("skipconfirmation") {
		log.Info("--- Review Effective Configuration (Images Command) ---")

		// 1. Global Settings (Relevant to Images)
		globalSettings := map[string]interface{}{
			"SavePath":            viper.GetString("savepath"),
			"OutputDir":           viper.GetString("images.output_dir"), // Display explicit output dir
			"ApiKeySet":           viper.GetString("apikey") != "",      // Show if API key is present
			"ApiClientTimeoutSec": viper.GetInt("apiclienttimeoutsec"),
			"ApiDelayMs":          viper.GetInt("apidelayms"),
			"LogApiRequests":      viper.GetBool("logapirequests"),
			"Concurrency":         viper.GetInt("images.concurrency"), // Show image-specific concurrency
		}
		globalJSON, _ := json.MarshalIndent(globalSettings, "  ", "  ")
		fmt.Println("  --- Global Settings (Relevant to Images) ---")
		fmt.Println("  " + strings.ReplaceAll(string(globalJSON), "\n", "\n  "))

		// 2. Image API Parameters
		imageAPIParams := map[string]interface{}{
			"ModelID":        viper.GetInt("images.modelId"),
			"ModelVersionID": viper.GetInt("images.modelVersionId"),
			"PostID":         viper.GetInt("images.postId"),
			"Username":       viper.GetString("images.username"),
			"Limit":          viper.GetInt("images.limit"),
			"Period":         viper.GetString("images.period"),
			"Sort":           viper.GetString("images.sort"),
			"NSFW":           viper.GetString("images.nsfw"),
			"MaxPages":       viper.GetInt("images.max_pages"),
			"SaveMetadata":   viper.GetBool("images.metadata"),
		}
		apiParamsJSON, _ := json.MarshalIndent(imageAPIParams, "  ", "  ")
		fmt.Println("\n  --- Image API Parameters ---")
		fmt.Println("  " + strings.ReplaceAll(string(apiParamsJSON), "\n", "\n  "))

		// Confirmation Prompt
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("\nProceed with these settings? (y/N): ")
		input, _ := reader.ReadString('\n')
		input = strings.ToLower(strings.TrimSpace(input))

		if input != "y" {
			log.Info("Operation cancelled by user.")
			os.Exit(0)
		}
		log.Info("Configuration confirmed.")
	} else {
		log.Info("Skipping configuration review due to --yes flag or config setting.")
	}
	// --- Display Effective Config & Confirm --- END ---

	// Add log to confirm concurrency level
	log.Infof("Using image download concurrency level: %d", numWorkers)

	// Default output dir if not provided
	if targetDir == "" {
		if globalConfig.SavePath == "" {
			log.Fatal("Required configuration 'SavePath' is not set and --output-dir flag was not provided.")
		}
		targetDir = filepath.Join(globalConfig.SavePath, "images")
		log.Infof("Output directory not specified, using default: %s", targetDir)
	}

	// Validate flags
	if modelID == 0 && modelVersionID == 0 && username == "" {
		log.Fatal("At least one of --model-id, --model-version-id, or --username must be provided")
	}
	if modelVersionID != 0 {
		log.Infof("Filtering images by Model Version ID: %d (overrides --model-id)", modelVersionID)
		modelID = 0
	}

	// --- API Client Setup (standard http client) ---
	if globalHttpTransport == nil {
		log.Warn("Global HTTP transport not initialized, using default.")
		globalHttpTransport = http.DefaultTransport
	}
	apiClient := &http.Client{
		Transport: globalHttpTransport,
		Timeout:   time.Duration(globalConfig.ApiClientTimeoutSec) * time.Second,
	}

	// --- Fetch Image List ---
	log.Info("Fetching image list from Civitai API...")

	var allImages []models.ImageApiItem
	baseURL := "https://civitai.com/api/v1/images"
	params := url.Values{}
	userTotalLimit := viper.GetInt("images.limit") // User's intended total limit (0 = unlimited)

	if modelVersionID != 0 {
		params.Set("modelVersionId", strconv.Itoa(modelVersionID))
	} else if modelID != 0 {
		params.Set("modelId", strconv.Itoa(modelID))
	} else if username != "" {
		params.Set("username", username)
	} else if postID != 0 {
		params.Set("postId", strconv.Itoa(postID))
	}

	// Use API default/max limit per page (e.g., 100 or 200) for efficiency.
	// Do NOT send the user's total limit here.
	params.Set("limit", "100") // Request a reasonable number per page

	// These parameters are still valid API parameters to send
	if period != "" {
		params.Set("period", period)
	}
	if sort != "" {
		params.Set("sort", sort)
	}
	if nsfw != "" {
		params.Set("nsfw", nsfw)
	}

	pageCount := 0
	var nextCursor string
	var loopErr error

	log.Info("--- Starting Image Fetching ---")

	for {
		pageCount++
		if maxPages > 0 && pageCount > maxPages {
			log.Infof("Reached max pages limit (%d). Stopping.", maxPages)
			break
		}

		currentParams := params
		if nextCursor != "" {
			currentParams.Set("cursor", nextCursor)
		}
		requestURL := baseURL + "?" + currentParams.Encode()

		log.Debugf("Requesting Image URL (Page %d inferred, Cursor: %s): %s", pageCount, nextCursor, requestURL)

		// --- Check for debug flag --- NEW
		if printUrl, _ := cmd.Flags().GetBool("debug-print-api-url"); printUrl {
			fmt.Println(requestURL) // Print only the URL to stdout
			os.Exit(0)              // Exit immediately
		}
		// --- End check for debug flag --- NEW

		req, err := http.NewRequest("GET", requestURL, nil)
		if err != nil {
			loopErr = fmt.Errorf("failed to create request for page %d: %w", pageCount, err)
			break
		}
		if globalConfig.ApiKey != "" {
			req.Header.Add("Authorization", "Bearer "+globalConfig.ApiKey)
		}

		resp, err := apiClient.Do(req)
		if err != nil {
			if urlErr, ok := err.(*url.Error); ok && urlErr.Timeout() {
				log.WithError(err).Warnf("Timeout fetching image metadata page %d. Retrying after delay...", pageCount)
				time.Sleep(5 * time.Second)
				continue
			}
			loopErr = fmt.Errorf("failed to fetch image metadata page %d: %w", pageCount, err)
			break
		}

		bodyBytes, readErr := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.WithError(closeErr).Warn("Error closing image API response body")
		}

		if readErr != nil {
			loopErr = fmt.Errorf("failed to read response body (Page %d): %w", pageCount, readErr)
			break
		}

		if resp.StatusCode != http.StatusOK {
			errMsg := fmt.Sprintf("Image API request failed (Page %d inferred) with status %s", pageCount, resp.Status)
			if len(bodyBytes) > 0 {
				maxLen := 200
				bodyStr := string(bodyBytes)
				if len(bodyStr) > maxLen {
					bodyStr = bodyStr[:maxLen] + "..."
				}
				errMsg += fmt.Sprintf(". Response: %s", bodyStr)
			}
			log.Error(errMsg)
			if resp.StatusCode == http.StatusTooManyRequests {
				log.Warn("Rate limited. Applying longer delay...")
				delay := time.Duration(globalConfig.ApiDelayMs)*time.Millisecond*5 + 5*time.Second
				time.Sleep(delay)
				continue
			}
			loopErr = errors.New(errMsg)
			break
		}

		var response models.ImageApiResponse
		if err := json.Unmarshal(bodyBytes, &response); err != nil {
			loopErr = fmt.Errorf("failed to decode image API response (Page %d): %w", pageCount, err)
			log.WithError(err).Errorf("Response body sample: %s", string(bodyBytes[:min(len(bodyBytes), 200)]))
			break
		}

		if len(response.Items) == 0 {
			log.Info("Received empty items list from API. Assuming end of results.")
			break
		}

		log.Infof("Received %d images from API page %d. Total collected: %d", len(response.Items), pageCount, len(allImages))
		allImages = append(allImages, response.Items...)

		// --- Check Total Limit --- START ---
		if userTotalLimit > 0 && len(allImages) >= userTotalLimit {
			log.Infof("Reached total image limit (%d). Stopping image fetching.", userTotalLimit)
			allImages = allImages[:userTotalLimit] // Truncate to exact limit
			break                                  // Stop fetching more pages
		}
		// --- Check Total Limit --- END ---

		nextCursor = response.Metadata.NextCursor
		if nextCursor == "" {
			log.Info("No next cursor found. Finished fetching.")
			break
		}

		log.Debugf("Next cursor found: %s", nextCursor)

		if globalConfig.ApiDelayMs > 0 {
			log.Debugf("Applying API delay: %d ms", globalConfig.ApiDelayMs)
			time.Sleep(time.Duration(globalConfig.ApiDelayMs) * time.Millisecond)
		}
	}

	if loopErr != nil {
		log.WithError(loopErr).Error("Image fetching stopped due to error.")
		if len(allImages) == 0 {
			log.Fatal("Exiting as no images were fetched before the error.")
		}
		log.Warnf("Proceeding with %d images fetched before the error.", len(allImages))
	} else {
		log.Info("--- Finished Image Fetching ---")
	}

	if len(allImages) == 0 {
		log.Info("No images found matching the criteria after fetching.")
		return
	}
	log.Infof("Found %d total images to potentially download.", len(allImages))

	// --- Initialize Bleve Index --- START ---
	// Use targetDir as base for index path, ensuring it's consistent
	indexPath := globalConfig.BleveIndexPath
	if indexPath == "" {
		indexPath = filepath.Join(targetDir, "civitai_images.bleve") // Default if config is empty
		log.Warnf("BleveIndexPath not set in config, defaulting index path for image downloads to: %s", indexPath)
	} else {
		// If a shared index path is provided, images might go into the same index
		// Or we could append a sub-directory like "images"? For now, use the path directly.
		// Example: If BleveIndexPath = /path/to/index, index will be at /path/to/index
	}
	log.Infof("Opening/Creating Bleve index at: %s", indexPath)
	bleveIndex, err := index.OpenOrCreateIndex(indexPath)
	if err != nil {
		log.Fatalf("Failed to open or create Bleve index: %v", err)
	}
	defer func() {
		log.Info("Closing Bleve index.")
		if err := bleveIndex.Close(); err != nil {
			log.Errorf("Error closing Bleve index: %v", err)
		}
	}()
	log.Info("Bleve index opened successfully.")
	// --- Initialize Bleve Index --- END ---

	// --- Downloader Setup ---
	downloadClient := &http.Client{
		Transport: globalHttpTransport,
		Timeout:   0,
	}
	dl := downloader.NewDownloader(downloadClient, globalConfig.ApiKey)

	// --- Target Directory ---
	finalBaseTargetDir := targetDir
	log.Infof("Ensuring base target directory exists: %s", finalBaseTargetDir)
	if err := os.MkdirAll(finalBaseTargetDir, 0750); err != nil {
		log.WithError(err).Fatalf("Failed to create base target directory: %s", finalBaseTargetDir)
	}

	// --- Download Workers ---
	var wg sync.WaitGroup
	jobs := make(chan imageJob, len(allImages))
	writer := uilive.New()
	writer.Start()

	var successCount int64
	var failureCount int64

	log.Infof("Starting %d image download workers...", numWorkers)
	for w := 1; w <= numWorkers; w++ {
		wg.Add(1)
		go imageDownloadWorker(w, jobs, dl, &wg, writer, &successCount, &failureCount, saveMeta, finalBaseTargetDir, bleveIndex)
	}

	// --- Queue Jobs ---
	log.Info("Queueing image download jobs...")
	queuedCount := 0
	for _, image := range allImages {
		if image.URL == "" {
			log.Warnf("Image ID %d has no URL, skipping.", image.ID)
			continue
		}

		job := imageJob{
			SourceURL: image.URL,
			ImageID:   image.ID,
			Metadata:  image,
		}
		jobs <- job
		queuedCount++
	}
	close(jobs)
	log.Infof("Queued %d image jobs.", queuedCount)

	// --- Wait for Completion ---
	log.Info("Waiting for image download workers to finish...")
	wg.Wait()
	writer.Stop()

	// --- Final Report ---
	finalSuccessCount := atomic.LoadInt64(&successCount)
	finalFailureCount := atomic.LoadInt64(&failureCount)

	log.Infof("Image download process completed.")
	log.Infof("Successfully downloaded: %d images", finalSuccessCount)
	log.Infof("Failed to download: %d images", finalFailureCount)

	if finalFailureCount > 0 {
		log.Warn("Some image downloads failed. Check logs for details.")
	}

	fmt.Println("----- Download Summary -----")
	fmt.Printf(" Target Base Directory: %s\n", finalBaseTargetDir)
	fmt.Printf(" Total Images Found API: %d\n", len(allImages))
	fmt.Printf(" Images Queued: %d\n", queuedCount)
	fmt.Printf(" Successfully Downloaded: %d\n", finalSuccessCount)
	fmt.Printf(" Failed Downloads: %d\n", finalFailureCount)
	fmt.Printf(" Metadata Saved: %t\n", saveMeta)
	fmt.Println("--------------------------")
}
