package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go-civitai-download/internal/database"
	"go-civitai-download/internal/downloader"
	"go-civitai-download/internal/models"

	log "github.com/sirupsen/logrus"
)

// --- Structs for Concurrent Image Downloads --- START ---
type imageDownloadJob struct {
	SourceURL   string
	TargetPath  string
	ImageID     int    // Keep ID for logging
	LogFilename string // Keep base filename for logging
}

// --- Structs for Concurrent Image Downloads --- END ---

// --- Worker for Concurrent Image Downloads --- START ---
func imageDownloadWorkerInternal(id int, jobs <-chan imageDownloadJob, imageDownloader *downloader.Downloader, wg *sync.WaitGroup, successCounter *int64, failureCounter *int64, logPrefix string) {
	defer wg.Done()
	log.Debugf("[%s-Worker-%d] Starting internal image worker", logPrefix, id)
	for job := range jobs {
		log.Debugf("[%s-Worker-%d] Received job for image ID %d -> %s", logPrefix, id, job.ImageID, job.TargetPath)

		// Check if image exists already
		if _, statErr := os.Stat(job.TargetPath); statErr == nil {
			log.Debugf("[%s-Worker-%d] Skipping image %s - already exists.", logPrefix, id, job.LogFilename)
			continue
		} else if !os.IsNotExist(statErr) {
			log.WithError(statErr).Warnf("[%s-Worker-%d] Failed to check status of target image file %s. Skipping.", logPrefix, id, job.TargetPath)
			atomic.AddInt64(failureCounter, 1)
			continue
		}

		// Download the image
		log.Debugf("[%s-Worker-%d] Downloading image %s from %s", logPrefix, id, job.LogFilename, job.SourceURL)
		_, dlErr := imageDownloader.DownloadFile(job.TargetPath, job.SourceURL, models.Hashes{}, 0)

		if dlErr != nil {
			log.WithError(dlErr).Errorf("[%s-Worker-%d] Failed to download image %s from %s", logPrefix, id, job.LogFilename, job.SourceURL)
			atomic.AddInt64(failureCounter, 1)
		} else {
			log.Debugf("[%s-Worker-%d] Downloaded image %s successfully.", logPrefix, id, job.LogFilename)
			atomic.AddInt64(successCounter, 1)
		}
	}
	log.Debugf("[%s-Worker-%d] Finishing internal image worker", logPrefix, id)
}

// --- Worker for Concurrent Image Downloads --- END ---

// processPage filters downloads based on config and database status.
// It returns the list of downloads that should be queued and their total size.
func processPage(db *database.DB, pageDownloads []potentialDownload, cfg *models.Config) ([]potentialDownload, uint64) {
	downloadsToQueue := []potentialDownload{}
	var queuedSizeBytes uint64 = 0

	for _, pd := range pageDownloads {
		// Calculate DB Key using ModelVersion ID
		if pd.CleanedVersion.ID == 0 {
			log.Warnf("Skipping potential download %s for model %s - missing ModelVersion ID.", pd.File.Name, pd.ModelName)
			continue
		}
		// Use prefix "v_" to distinguish version keys
		dbKey := fmt.Sprintf("v_%d", pd.CleanedVersion.ID)

		// Check database
		// Get retrieves raw bytes, unmarshaling happens later if needed
		rawValue, err := db.Get([]byte(dbKey)) // Note: db.Get returns raw bytes

		shouldQueue := false
		// Use errors.Is to check for the specific ErrNotFound error from our database package
		if errors.Is(err, database.ErrNotFound) {
			log.Debugf("Model Version %d (Key: %s) not found in DB. Queuing for download.", pd.CleanedVersion.ID, dbKey)
			shouldQueue = true
			// Create initial entry using the correct DatabaseEntry fields
			newEntry := models.DatabaseEntry{
				ModelName:    pd.ModelName,
				ModelType:    pd.ModelType,
				Version:      pd.CleanedVersion,                // Store the cleaned version struct
				File:         pd.File,                          // Store the file struct
				Timestamp:    time.Now().Unix(),                // Use Unix timestamp for AddedAt
				Creator:      pd.Creator,                       // Store the creator struct
				Filename:     filepath.Base(pd.TargetFilepath), // Use the calculated filename
				Folder:       pd.Slug,                          // Use the calculated folder slug
				Status:       models.StatusPending,             // Use constant
				ErrorDetails: "",                               // Use correct field name
			}
			// Marshal the new entry to JSON before putting into DB
			entryBytes, marshalErr := json.Marshal(newEntry)
			if marshalErr != nil {
				log.WithError(marshalErr).Errorf("Failed to marshal new DB entry for key %s", dbKey)
				continue // Skip queuing if marshalling fails
			}
			// Put the marshalled bytes
			if errPut := db.Put([]byte(dbKey), entryBytes); errPut != nil {
				log.WithError(errPut).Errorf("Failed to add pending entry to DB for key %s", dbKey)
				// Decide if we should still attempt download? Maybe not.
				continue // Skip queuing if DB write fails
			}
		} else if err != nil {
			// Handle other potential DB errors during Get
			log.WithError(err).Errorf("Error checking database for key %s", dbKey)
			continue // Skip this file on DB error
		} else {
			// Entry exists, unmarshal it
			var entry models.DatabaseEntry
			if unmarshalErr := json.Unmarshal(rawValue, &entry); unmarshalErr != nil {
				log.WithError(unmarshalErr).Errorf("Failed to unmarshal existing DB entry for key %s", dbKey)
				continue // Skip if we can't parse the existing entry
			}

			log.Debugf("Model Version %d (Key: %s) found in DB with status: %s", entry.Version.ID, dbKey, entry.Status)
			switch entry.Status {
			case models.StatusDownloaded:
				log.Debugf("DB Status for %s (VersionID: %d, Key: %s) is Downloaded. Checking filesystem...", pd.FinalBaseFilename, pd.CleanedVersion.ID, dbKey)
				// Check if the file *actually* exists on disk
				if _, statErr := os.Stat(pd.TargetFilepath); os.IsNotExist(statErr) {
					// File is missing despite DB saying downloaded!
					log.Warnf("File %s marked as downloaded in DB (Key: %s), but not found on disk! Re-queuing.", pd.TargetFilepath, dbKey)
					shouldQueue = true
					// Update status back to Pending and clear error
					entry.Status = models.StatusPending
					entry.ErrorDetails = ""
					// Update other fields that might change
					entry.Folder = pd.Slug
					entry.Filename = filepath.Base(pd.TargetFilepath)
					entry.Version = pd.CleanedVersion
					entry.File = pd.File
					// Update DB entry to reflect Pending status
					entryBytes, marshalErr := json.Marshal(entry)
					if marshalErr != nil {
						log.WithError(marshalErr).Errorf("Failed to marshal entry for re-queue update (missing file) %s", dbKey)
						shouldQueue = false // Don't queue if marshalling fails
					} else if errUpdate := db.Put([]byte(dbKey), entryBytes); errUpdate != nil {
						log.WithError(errUpdate).Errorf("Failed to update DB entry to Pending (missing file) for key %s", dbKey)
						shouldQueue = false // Don't queue if update fails
					}
					// End of handling missing file
				} else if statErr == nil {
					// File *does* exist, proceed with original skip logic + metadata check
					log.Infof("Skipping %s (VersionID: %d, Key: %s) - File exists and DB status is Downloaded.", pd.TargetFilepath, pd.CleanedVersion.ID, dbKey)
					// Update fields that might change between runs
					entry.Folder = pd.Slug
					entry.Filename = filepath.Base(pd.TargetFilepath)
					entry.Version = pd.CleanedVersion // Update associated metadata version
					entry.File = pd.File              // Update file details (URL might change)

					// --- START: Save Metadata Check for Existing Download ---
					if cfg.SaveMetadata {
						// Calculate the expected *final* path including the prepended ID
						expectedFilename := fmt.Sprintf("%d_%s", pd.ModelVersionID, filepath.Base(pd.TargetFilepath))
						expectedFinalPath := filepath.Join(filepath.Dir(pd.TargetFilepath), expectedFilename)
						// Derive metadata path from the expected final path
						metadataPath := strings.TrimSuffix(expectedFinalPath, filepath.Ext(expectedFinalPath)) + ".json"

						if _, metaStatErr := os.Stat(metadataPath); os.IsNotExist(metaStatErr) {
							log.Infof("Model file exists, but metadata %s is missing. Saving metadata.", filepath.Base(metadataPath))
							// Marshal the *updated* entry.Version (which is pd.CleanedVersion)
							jsonData, jsonErr := json.MarshalIndent(entry.Version, "", "  ")
							if jsonErr != nil {
								log.WithError(jsonErr).Warnf("Failed to marshal version metadata for existing file %s", pd.TargetFilepath)
							} else {
								if writeErr := os.WriteFile(metadataPath, jsonData, 0644); writeErr != nil {
									log.WithError(writeErr).Warnf("Failed to write version metadata file %s", metadataPath)
								}
							}
						} else if metaStatErr != nil {
							// Log error if stating metadata file failed for other reasons
							log.WithError(metaStatErr).Warnf("Could not check status of metadata file %s", metadataPath)
						}
					}
					// --- END: Save Metadata Check for Existing Download ---

					// Update the entry in the database (keeping status Downloaded)
					entryBytes, marshalErr := json.Marshal(entry)
					if marshalErr != nil {
						log.WithError(marshalErr).Warnf("Failed to marshal updated downloaded entry %s", dbKey)
					} else if errUpdate := db.Put([]byte(dbKey), entryBytes); errUpdate != nil {
						log.WithError(errUpdate).Warnf("Failed to update metadata for downloaded entry %s", dbKey)
					}
					shouldQueue = false
				} else {
					// Some other error occurred when checking file existence
					log.WithError(statErr).Warnf("Error checking filesystem for %s (Key: %s). Skipping queue.", pd.TargetFilepath, dbKey)
					shouldQueue = false
					// Optionally update DB entry here too, or just skip?
				}
			case models.StatusPending, models.StatusError:
				log.Infof("Re-queuing %s (VersionID: %d, Key: %s) - Status is %s.", pd.TargetFilepath, pd.CleanedVersion.ID, dbKey, entry.Status)
				shouldQueue = true
				// Update status back to Pending and clear error if any
				entry.Status = models.StatusPending
				entry.ErrorDetails = ""
				// Update fields that might change
				entry.Folder = pd.Slug
				entry.Filename = filepath.Base(pd.TargetFilepath)
				entry.Version = pd.CleanedVersion
				entry.File = pd.File
				// entry.Timestamp = time.Now().Unix() // Optionally update timestamp?

				entryBytes, marshalErr := json.Marshal(entry)
				if marshalErr != nil {
					log.WithError(marshalErr).Errorf("Failed to marshal entry for re-queue update %s", dbKey)
					shouldQueue = false // Don't queue if marshalling fails
				} else if errUpdate := db.Put([]byte(dbKey), entryBytes); errUpdate != nil {
					log.WithError(errUpdate).Errorf("Failed to update DB entry to Pending for key %s", dbKey)
					shouldQueue = false // Don't queue if update fails
				}
			default:
				log.Warnf("Skipping %s (VersionID: %d, Key: %s) - Unknown status '%s' in database.", pd.TargetFilepath, pd.CleanedVersion.ID, dbKey, entry.Status)
				shouldQueue = false
			}
		}

		if shouldQueue {
			downloadsToQueue = append(downloadsToQueue, pd)
			queuedSizeBytes += uint64(pd.File.SizeKB * 1024)
			log.Debugf("Added confirmed download to queue: %s (Model: %s)", pd.File.Name, pd.ModelName)
		}
	}

	return downloadsToQueue, queuedSizeBytes
}

// saveModelInfoFile saves the full model metadata to a .json file.
// It saves the file to basePath/model_info/{baseModelSlug}/{modelNameSlug}/{model.ID}.json.
func saveModelInfoFile(model models.Model, basePath string, baseModelSlug string, modelNameSlug string) error {
	// Construct the directory path including base model and model name slugs
	infoDirPath := filepath.Join(basePath, "model_info", baseModelSlug, modelNameSlug)

	// Ensure the directory exists
	if err := os.MkdirAll(infoDirPath, 0700); err != nil {
		log.WithError(err).Errorf("Failed to create model info directory: %s", infoDirPath)
		return fmt.Errorf("failed to create directory %s: %w", infoDirPath, err)
	}

	// Construct the file path
	filePath := filepath.Join(infoDirPath, fmt.Sprintf("%d.json", model.ID))

	// Marshal the full model info
	jsonData, jsonErr := json.MarshalIndent(model, "", "  ")
	if jsonErr != nil {
		log.WithError(jsonErr).Warnf("Failed to marshal full model info for model %d (%s)", model.ID, model.Name)
		return fmt.Errorf("failed to marshal model info for %d: %w", model.ID, jsonErr)
	}

	// Write the file (overwrite if exists)
	if writeErr := os.WriteFile(filePath, jsonData, 0600); writeErr != nil {
		log.WithError(writeErr).Warnf("Failed to write model info file %s", filePath)
		return fmt.Errorf("failed to write model info file %s: %w", filePath, writeErr)
	}

	log.Debugf("Saved full model info to %s", filePath)
	return nil
}

// downloadImages handles downloading a list of images concurrently to a specified directory.
func downloadImages(logPrefix string, images []models.ModelImage, baseDir string, imageDownloader *downloader.Downloader, numWorkers int) (finalSuccessCount, finalFailCount int) {
	if imageDownloader == nil {
		log.Warnf("[%s] Image downloader is nil, cannot download images.", logPrefix)
		return 0, len(images) // Count all as failed if downloader doesn't exist
	}
	if len(images) == 0 {
		log.Debugf("[%s] No images provided to download.", logPrefix)
		return 0, 0
	}
	if numWorkers <= 0 {
		log.Warnf("[%s] Invalid concurrency level %d for image download, defaulting to 1.", logPrefix, numWorkers)
		numWorkers = 1
	}

	log.Infof("[%s] Attempting concurrent download for %d images to %s (Concurrency: %d)", logPrefix, len(images), baseDir, numWorkers)

	if err := os.MkdirAll(baseDir, 0755); err != nil {
		log.WithError(err).Errorf("[%s] Failed to create base directory for images: %s", logPrefix, baseDir)
		return 0, len(images) // Cannot proceed, count all as failed
	}

	// --- Setup Concurrency ---
	jobs := make(chan imageDownloadJob, numWorkers*2) // Buffered channel
	var wg sync.WaitGroup
	var successCounter int64 = 0
	var failureCounter int64 = 0

	// --- Start Workers ---
	log.Debugf("[%s] Starting %d internal image download workers...", logPrefix, numWorkers)
	for w := 1; w <= numWorkers; w++ {
		wg.Add(1)
		go imageDownloadWorkerInternal(w, jobs, imageDownloader, &wg, &successCounter, &failureCounter, logPrefix)
	}

	// --- Queue Jobs --- Loop through images and send jobs
	queuedCount := 0
	for imgIdx, image := range images {
		// Construct image filename: {imageID}.{ext} (Copied from previous sequential logic)
		imgUrlParsed, urlErr := url.Parse(image.URL)
		var imgFilename string
		if urlErr != nil || image.ID == 0 {
			log.WithError(urlErr).Warnf("[%s] Cannot determine filename/ID for image %d (URL: %s). Using index.", logPrefix, imgIdx, image.URL)
			imgFilename = fmt.Sprintf("image_%d.jpg", imgIdx) // Fallback
		} else {
			ext := filepath.Ext(imgUrlParsed.Path)
			if ext == "" || len(ext) > 5 { // Basic check for valid extension
				log.Warnf("[%s] Image URL %s has unusual/missing extension '%s', defaulting to .jpg", logPrefix, image.URL, ext)
				ext = ".jpg"
			}
			imgFilename = fmt.Sprintf("%d%s", image.ID, ext)
		}
		imgTargetPath := filepath.Join(baseDir, imgFilename)

		// Create and send job
		job := imageDownloadJob{
			SourceURL:   image.URL,
			TargetPath:  imgTargetPath,
			ImageID:     image.ID,
			LogFilename: imgFilename, // Pass for consistent logging
		}
		log.Debugf("[%s] Queueing image job: ID %d -> %s", logPrefix, job.ImageID, job.TargetPath)
		jobs <- job
		queuedCount++
	}

	close(jobs) // Signal no more jobs
	log.Debugf("[%s] All %d image jobs queued. Waiting for workers...", logPrefix, queuedCount)

	// --- Wait for Completion ---
	wg.Wait()
	log.Infof("[%s] Image download complete. Success: %d, Failed: %d", logPrefix, atomic.LoadInt64(&successCounter), atomic.LoadInt64(&failureCounter))

	return int(atomic.LoadInt64(&successCounter)), int(atomic.LoadInt64(&failureCounter))
}
