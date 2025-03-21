package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	// Import the new gcpupload package
	"MediaProcessor/gcpupload" // Use the full module path
)

// Config struct to hold configuration
type Config struct {
	EnableGCPUpload          bool   `json:"enable_gcp_upload"`
	DeleteLocalAfterUpload   bool   `json:"delete_local_after_upload"`
	GCPBucketName            string `json:"gcp_bucket_name"`
	GCPBucketPath            string `json:"gcp_bucket_path"`
	GCPServiceAccountKeyPath string `json:"gcp_service_account_key_path"`
	Resolutions              []int  `json:"video_resolutions"`
	ImageSize                int    `json:"image_size"`
	VideoSize                int    `json:"video_size"`
	VideoCRF                 int    `json:"video_crf"`
	NumberOfWorkers          int    `json:"workers"`
}

// SignedURLRequest structure
type SignedURLRequest struct {
	ObjectName        string `json:"object_name"`
	ContentType       string `json:"content_type"`
	ExpirationMinutes int    `json:"expiration_minutes"`
}

// SignedURLResponse structure
type SignedURLResponse struct {
	URL   string `json:"url"`
	Error string `json:"error,omitempty"`
}

// BatchSignedURLRequest structure
type BatchSignedURLRequest struct {
	ObjectNames       []string `json:"object_names"`
	ContentType       string   `json:"content_type"`
	ExpirationMinutes int      `json:"expiration_minutes"`
}

// BatchSignedURLResponse structure
type BatchSignedURLResponse struct {
	URLs  map[string]string `json:"urls"`
	Error string            `json:"error,omitempty"`
}

// FileCleaner manages files that will be deleted after processing
type FileCleaner struct {
	mu          sync.Mutex
	pendingRefs map[string]int       // Reference count for each file
	inUseFiles  map[string]time.Time // Files currently in use with timestamp
}

// Global file cleaner
var fileCleaner = &FileCleaner{
	pendingRefs: make(map[string]int),
	inUseFiles:  make(map[string]time.Time),
}

// Add these methods to FileCleaner
// MarkFileInUse marks a file as currently being processed
func (fc *FileCleaner) MarkFileInUse(filePath string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.inUseFiles[filePath] = time.Now()
	log.Printf("Marked file as in use: %s", filePath)
}

// MarkFileDone marks a file as no longer being processed
func (fc *FileCleaner) MarkFileDone(filePath string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	delete(fc.inUseFiles, filePath)
	log.Printf("Marked file as done: %s", filePath)
}

// IsFileInUse checks if a file is currently being processed
func (fc *FileCleaner) IsFileInUse(filePath string) bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	_, exists := fc.inUseFiles[filePath]
	return exists
}

// RegisterFile adds a file to the tracking system with initial reference count
func (fc *FileCleaner) RegisterFile(filePath string, initialRefs int) {
	if !config.EnableGCPUpload || !config.DeleteLocalAfterUpload {
		return
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()

	// Initialize or increment existing reference count
	fc.pendingRefs[filePath] = initialRefs
	log.Printf("Registered file for cleanup: %s (refs: %d)", filePath, initialRefs)
}

// ReleaseFile decrements the reference count for a file and deletes it if zero
// Modified ReleaseFile to check if file is in use
func (fc *FileCleaner) ReleaseFile(filePath string) {
	if !config.EnableGCPUpload || !config.DeleteLocalAfterUpload {
		return
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()

	count, exists := fc.pendingRefs[filePath]
	if !exists {
		log.Printf("Warning: Attempt to release untracked file: %s", filePath)
		return
	}

	count--
	if count <= 0 {
		// Check if file is currently in use elsewhere
		if _, inUse := fc.inUseFiles[filePath]; inUse {
			log.Printf("File %s has 0 references but is currently in use. Delaying deletion.", filePath)
			// Keep a minimum reference count of 1 while the file is in use
			fc.pendingRefs[filePath] = 1
			return
		}

		log.Printf("Deleting local file: %s (no more references and not in use)", filePath)
		if err := os.Remove(filePath); err != nil {
			// If file doesn't exist, it might have been deleted by a different process
			if !os.IsNotExist(err) {
				log.Printf("Warning: Failed to delete local file %s: %v", filePath, err)
			}
		}
		delete(fc.pendingRefs, filePath)
	} else {
		// Update reference count
		fc.pendingRefs[filePath] = count
		log.Printf("Released reference to file: %s (remaining refs: %d)", filePath, count)
	}
}

// DeleteFolder removes a directory safely
func (fc *FileCleaner) DeleteFolder(folderPath string) {
	if !config.EnableGCPUpload || !config.DeleteLocalAfterUpload {
		return
	}

	log.Printf("Deleting local directory: %s", folderPath)
	if err := os.RemoveAll(folderPath); err != nil {
		log.Printf("Warning: Failed to delete local directory: %v", err)
	}
}

// Helper function to copy a file
func copyFile(src, dst string) error {
	// Read all content from source file
	content, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	// Write content to destination file
	err = os.WriteFile(dst, content, 0644)
	if err != nil {
		return err
	}

	log.Printf("Successfully copied %s to %s", src, dst)
	return nil
}

// Global variables
var (
	config      Config
	gcsUploader *gcpupload.GCSUploader
	natsConn    *nats.Conn
	natsOnce    sync.Once
)

// // Transcoding resolutions
// var resolutions = []int{5760, 2880, 2160, 1440, 1088, 720, 480}

// Request structure
type TranscodeRequest struct {
	ImgInputFile   string `json:"imageinput_file"`
	InputFile      string `json:"input_file"`
	AlphaInputFile string `json:"alphainput_file"`
	VideoURL       string `json:"video_url"`
	ImageURL       string `json:"image_url"`
	AlphaVideoURL  string `json:"alphavideo_url"`
	// New fields for tracking and callback
	ContentID      string `json:"content_id,omitempty"`
	CallbackURL    string `json:"callback_url,omitempty"`
	CallbackTopic  string `json:"callback_topic,omitempty"`
	OrganizationID string `json:"organization_id,omitempty"`
}

// MediaProcessResult represents the result from media processing
type MediaProcessResult struct {
	ContentID      string `json:"content_id,omitempty"`
	OriginalURL    string `json:"original_url"`
	ProcessedURL   string `json:"processed_url"`
	HlsURL         string `json:"hls_url,omitempty"`     // Added HLS URL field
	DashURL        string `json:"dash_url,omitempty"`    // Added DASH URL field
	MediaType      string `json:"media_type"`            // "image", "video", "object_3d"
	ProcessingType string `json:"processing_type"`       // "compressed", "hls", "dash", "alpha", "stitched"
	Orientation    string `json:"orientation,omitempty"` // Added orientation field
	HasAlpha       bool   `json:"has_alpha,omitempty"`   // Added has_alpha field
	Success        bool   `json:"success"`
	Error          string `json:"error,omitempty"`
	Timestamp      int64  `json:"timestamp"`
}

// Response structure
type TranscodeResponse struct {
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

// Load configuration from JSON file
func loadConfig(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	err = decoder.Decode(&config)
	if err != nil {
		return err
	}

	log.Println("Configuration loaded successfully!")
	return nil
}

// Function to send processing results back to the requester
func sendProcessingResult(result MediaProcessResult, request TranscodeRequest) {
	// If no ContentID is provided, there's no need to report back
	if request.ContentID == "" && request.CallbackURL == "" && request.CallbackTopic == "" {
		log.Printf("No ContentID or callback mechanism provided, skipping result notification")
		return
	}

	// Set the content ID from the request
	result.ContentID = request.ContentID
	result.Timestamp = time.Now().Unix()

	// Determine the appropriate callback method
	if request.CallbackURL != "" {
		// Option 1: HTTP callback
		log.Printf("Using HTTP callback to URL: %s", request.CallbackURL)
		sendHTTPCallback(result, request.CallbackURL)
	} else if request.CallbackTopic != "" {
		// Option 2: NATS callback to custom topic
		log.Printf("Using NATS callback to custom topic: %s", request.CallbackTopic)
		sendNATSCallback(result, request.CallbackTopic)
	} else if request.ContentID != "" {
		// Option 3: Default NATS callback to standard result topic
		defaultTopic := getDefaultResultTopic(result)
		log.Printf("Using NATS callback to default topic: %s for ContentID: %s",
			defaultTopic, request.ContentID)
		sendNATSCallback(result, defaultTopic)
	}
}

// Function to send results via HTTP
func sendHTTPCallback(result MediaProcessResult, callbackURL string) {
	// Create JSON payload
	jsonData, err := json.Marshal(result)
	if err != nil {
		log.Printf("Error marshaling result for HTTP callback: %v", err)
		return
	}

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	// Implement retry logic (3 attempts with exponential backoff)
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		// Create and send request
		req, err := http.NewRequest("POST", callbackURL, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Printf("Error creating HTTP callback request (attempt %d): %v", attempt+1, err)
			time.Sleep(time.Duration(1<<uint(attempt)) * time.Second) // Exponential backoff
			continue
		}

		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Error sending HTTP callback (attempt %d): %v", attempt+1, err)
			time.Sleep(time.Duration(1<<uint(attempt)) * time.Second) // Exponential backoff
			continue
		}

		// Check response
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			log.Printf("Successfully sent HTTP callback to %s", callbackURL)
			resp.Body.Close()
			return
		} else {
			log.Printf("HTTP callback failed with status: %s (attempt %d)", resp.Status, attempt+1)
			resp.Body.Close()
			time.Sleep(time.Duration(1<<uint(attempt)) * time.Second) // Exponential backoff
		}
	}

	log.Printf("Failed to send HTTP callback after %d attempts", maxRetries)
}

// Get NATS connection (singleton pattern)
func getNATSConnection() (*nats.Conn, error) {
	var err error
	natsOnce.Do(func() {
		// Get NATS URL from environment or use default
		natsURL := os.Getenv("NATS_URL")
		if natsURL == "" {
			natsURL = "nats://localhost:4222"
			log.Printf("NATS_URL not found in environment, using default: %s", natsURL)
		}

		// Connect to NATS with options for reconnection
		natsConn, err = nats.Connect(natsURL,
			nats.Name("MediaProcessor"),
			nats.ReconnectWait(time.Second*2),
			nats.MaxReconnects(10),
			nats.ErrorHandler(func(nc *nats.Conn, s *nats.Subscription, err error) {
				log.Printf("NATS error: %v", err)
			}),
		)
		if err != nil {
			log.Printf("Failed to connect to NATS: %v", err)
			return
		}

		log.Printf("Connected to NATS server at %s", natsURL)
	})

	return natsConn, err
}

// Function to send results via NATS
func sendNATSCallback(result MediaProcessResult, topic string) {
	// Get NATS connection
	nc, err := getNATSConnection()
	if err != nil {
		log.Printf("Error getting NATS connection for result publishing: %v", err)
		return
	}

	// Create JSON payload
	jsonData, err := json.Marshal(result)
	if err != nil {
		log.Printf("Error marshaling result for NATS callback: %v", err)
		return
	}

	// Implement retry logic (3 attempts with exponential backoff)
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		// Publish to NATS topic
		err = nc.Publish(topic, jsonData)
		if err != nil {
			log.Printf("Error publishing result to NATS topic %s (attempt %d): %v",
				topic, attempt+1, err)
			time.Sleep(time.Duration(1<<uint(attempt)) * time.Second) // Exponential backoff
			continue
		}

		// Flush to ensure the message is sent
		if err := nc.Flush(); err != nil {
			log.Printf("Error flushing NATS connection (attempt %d): %v", attempt+1, err)
			time.Sleep(time.Duration(1<<uint(attempt)) * time.Second) // Exponential backoff
			continue
		}

		log.Printf("Successfully published result to NATS topic: %s", topic)
		return
	}

	log.Printf("Failed to send NATS callback after %d attempts", maxRetries)
}

// Get the default result topic based on the processing type
func getDefaultResultTopic(result MediaProcessResult) string {
	switch {
	case result.MediaType == "image" && result.ProcessingType == "compressed":
		return "result.compressimage"
	case result.MediaType == "video" && result.ProcessingType == "compressed":
		return "result.compressvideo"
	case result.MediaType == "video" && (result.ProcessingType == "hls" || result.ProcessingType == "dash"):
		return "result.transcodehlsdash"
	case result.MediaType == "video" && result.ProcessingType == "alpha":
		return "result.generatealpha"
	case result.MediaType == "video" && result.ProcessingType == "stitched":
		return "result.stitchvideos"
	default:
		return "result.default"
	}
}

// Helper function to send error results
// func sendErrorResult(mediaType, processingType, originalURL, errorMessage string, req TranscodeRequest) {
// 	result := MediaProcessResult{
// 		OriginalURL:    originalURL,
// 		ProcessedURL:   "",
// 		MediaType:      mediaType,
// 		ProcessingType: processingType,
// 		Success:        false,
// 		Error:          errorMessage,
// 	}
// 	sendProcessingResult(result, req)
// }

func sendErrorResult(mediaType, processingType, originalURL, errorMessage string, req TranscodeRequest) {
	result := MediaProcessResult{
		ContentID:      req.ContentID,
		OriginalURL:    originalURL,
		ProcessedURL:   "",
		MediaType:      mediaType,
		ProcessingType: processingType,
		Success:        false,
		Error:          errorMessage,
		Timestamp:      time.Now().Unix(),
	}
	sendProcessingResult(result, req)
}

// Modified processExperience function to use separate file copies
func processExperience(req TranscodeRequest) (string, error) {
	var localImgFilePath string
	var localvideoFilePath string
	var localalpvideoFilePath string
	var err error
	var wg sync.WaitGroup
	var videoOrientation string // Single orientation variable

	// Process image
	if req.ImgInputFile != "" {
		// Handle local file
		localImgFilePath = req.ImgInputFile
		if _, err := os.Stat(localImgFilePath); os.IsNotExist(err) {
			return "", fmt.Errorf("input file not found")
		}
	} else if req.ImageURL != "" {
		// Handle URL file
		localImgFilePath, err = download_Video(req.ImageURL)
		if err != nil {
			return "", fmt.Errorf("failed to download image")
		}
	}
	// else {
	// 	return "", fmt.Errorf("missing imageinput_file or image_url parameter")
	// }

	// Process video
	if req.InputFile != "" {
		// Handle local file
		localvideoFilePath = req.InputFile
		if _, err := os.Stat(localvideoFilePath); os.IsNotExist(err) {
			return "", fmt.Errorf("input file not found")
		}
	} else if req.VideoURL != "" {
		// Handle URL file
		localvideoFilePath, err = download_Video(req.VideoURL)
		if err != nil {
			return "", fmt.Errorf("failed to download video")
		}
	} else {
		return "", fmt.Errorf("missing input_file or video_url parameter")
	}

	// Process alpha video if available
	if strings.TrimSpace(req.AlphaInputFile) != "" {
		// Handle local file
		localalpvideoFilePath = req.AlphaInputFile
		if _, err := os.Stat(localalpvideoFilePath); os.IsNotExist(err) {
			return "", fmt.Errorf("input file not found: %s", localalpvideoFilePath)
		}
	} else if strings.TrimSpace(req.AlphaVideoURL) != "" {
		// Handle URL file
		var err error
		localalpvideoFilePath, err = download_Video(req.AlphaVideoURL)
		if err != nil {
			return "", fmt.Errorf("failed to download video from URL: %s", req.AlphaVideoURL)
		}
	}

	//Stitch video
	var stitchedVideoPath string
	if strings.TrimSpace(localalpvideoFilePath) != "" {
		originalVideoPath := localvideoFilePath
		originalExt := filepath.Ext(localvideoFilePath)
		OutputFile := fmt.Sprintf("%s_stitched%s", strings.TrimSuffix(localvideoFilePath, originalExt), originalExt)
		var stitchOrientation string
		stitchedVideoPath, stitchOrientation, err = stitchVideos(localvideoFilePath, localalpvideoFilePath, OutputFile)
		if err != nil {
			return "", fmt.Errorf("video stitching failed: %v", err)
		}

		// Set the main orientation variable
		videoOrientation = stitchOrientation
		// Upload to GCS
		wg.Add(1)
		go func(stitchedPath string, orientation string) {
			defer wg.Done()

			// Upload to GCS only if enabled in config
			if config.EnableGCPUpload {
				if gcsUploader == nil {
					log.Printf("Error: GCS uploader not initialized")
					sendErrorResult("video", "stitched", req.VideoURL, "GCS uploader not initialized", req)
					return
				}

				path, err := uploadToGCS(stitchedPath, "", req)
				if err != nil {
					log.Printf("Error uploading stitched video file to GCS: %v", err)
					sendErrorResult("video", "stitched", req.VideoURL, fmt.Sprintf("Upload failed: %v", err), req)
				} else {
					log.Printf("Stitched video uploaded successfully to GCS: %v", path)

					// Send success result
					result := MediaProcessResult{
						OriginalURL:    req.VideoURL,
						ProcessedURL:   path,
						MediaType:      "video",
						ProcessingType: "stitched",
						Orientation:    orientation,
						HasAlpha:       true, // Always true for stitched videos
						Success:        true,
					}
					sendProcessingResult(result, req)

					// Delete original downloaded files if they were from URLs
					if config.DeleteLocalAfterUpload {
						if req.VideoURL != "" {
							log.Printf("Deleting downloaded video: %s", originalVideoPath)
							os.Remove(originalVideoPath)
						}
						if req.AlphaVideoURL != "" {
							log.Printf("Deleting downloaded alpha video: %s", localalpvideoFilePath)
							os.Remove(localalpvideoFilePath)
						}
					}
				}
			}
		}(stitchedVideoPath, videoOrientation)

		// Update the video path to use for further processing
		localvideoFilePath = stitchedVideoPath
		log.Printf("Original Stitched Video: %s", localvideoFilePath)
		// return "", fmt.Errorf("....Temporary STOP....")
	}

	// Check if the file is MOV format and extract alpha channel if needed
	var originalMOVPath string
	if checkFormat(localvideoFilePath, "mov") {
		log.Println("Detected MOV file. Extracting alpha channel...")
		originalMOVPath = localvideoFilePath
		var extractOrientation string
		localvideoFilePath, extractOrientation, err = processAlphaExtraction(originalMOVPath)
		if err != nil {
			sendErrorResult("video", "alpha", req.VideoURL, fmt.Sprintf("Alpha extraction failed: %v", err), req)
			return "", fmt.Errorf("alpha extraction failed: %v", err)
		}

		// Set the main orientation variable if not already set
		if videoOrientation == "" {
			videoOrientation = extractOrientation
		}

		// Register the alpha-extracted file
		// fileCleaner.RegisterFile(localFilePath, 1)
		os.Remove(originalMOVPath)
	}

	//Generate HLS and Dash
	// Create a copy of the video with a unique name specifically for HLS/Dash
	ext := filepath.Ext(localvideoFilePath)
	hlsDashCopyPath := fmt.Sprintf("%s_AS%s",
		strings.TrimSuffix(localvideoFilePath, ext),
		ext)

	// Make the copy
	err = copyFile(localvideoFilePath, hlsDashCopyPath)
	if err != nil {
		log.Printf("Error creating HLS/Dash copy: %v", err)
		sendErrorResult("video", "hls", req.VideoURL, fmt.Sprintf("HLS/Dash copy failed: %v", err), req)
		return "", fmt.Errorf("error creating HLS/Dash copy: %v", err)
	}
	log.Printf("Created dedicated copy for HLS/Dash: %s", hlsDashCopyPath)

	// Process the video asynchronously
	wg.Add(1)
	go func(originalVideoPath string, orientation string) {
		defer wg.Done()

		// Process this dedicated copy (which won't be touched by other processes)
		err = generateHlsDash(originalVideoPath)
		if err != nil {
			log.Printf("Error processing video: %v", err)
			sendErrorResult("video", "hls", req.VideoURL, fmt.Sprintf("HLS/Dash generation failed: %v", err), req)
			// Clean up the copy on error
			os.Remove(originalVideoPath)
			return
		}

		if config.EnableGCPUpload {
			if gcsUploader == nil {
				log.Printf("Error: GCS uploader not initialized")
				sendErrorResult("video", "hls", req.VideoURL, "GCS uploader not initialized", req)
				return
			}

			baseName := strings.TrimSuffix(filepath.Base(originalVideoPath), filepath.Ext(originalVideoPath))
			outputDir := baseName
			log.Printf("Uploading HLS/Dash to GCS: %s", outputDir)
			path, err := uploadToGCS(outputDir, "", req)
			if err != nil {
				log.Printf("Error uploading HLS/Dash to GCS: %v", err)
				sendErrorResult("video", "hls", req.VideoURL, fmt.Sprintf("HLS/Dash upload failed: %v", err), req)
			} else {
				var hasAlpha bool
				log.Printf("Uploaded HLS/Dash successfully to GCS: %v", path)

				// Construct the specific URLs for HLS and DASH files
				hlsFilePath := filepath.Join("transcoded", "h264_master.m3u8")
				dashFilePath := filepath.Join("transcoded", "h264_master.mpd")

				// Properly construct the full URLs
				hlsURL := fmt.Sprintf("%s/%s", path, hlsFilePath)
				dashURL := fmt.Sprintf("%s/%s", path, dashFilePath)

				log.Printf("HLS stream available at: %s", hlsURL)
				log.Printf("DASH stream available at: %s", dashURL)
				if orientation != "" {
					hasAlpha = true
				} else {
					hasAlpha = false
				}

				// Send success result
				result := MediaProcessResult{
					OriginalURL:    req.VideoURL,
					ProcessedURL:   path,
					HlsURL:         hlsURL,  // Add specific HLS URL
					DashURL:        dashURL, // Add specific DASH URL
					MediaType:      "video",
					ProcessingType: "hls",
					Orientation:    orientation,
					HasAlpha:       hasAlpha,
					Success:        true,
				}
				sendProcessingResult(result, req)
			}

			// Delete the dedicated copy now that processing is complete
			log.Printf("Deleting HLS/Dash dedicated copy: %s", originalVideoPath)
			os.Remove(originalVideoPath)
		}
	}(hlsDashCopyPath, videoOrientation)

	//Compress Video
	originalExt := filepath.Ext(localvideoFilePath)
	OutputFile := fmt.Sprintf("%s_Compressed%s", strings.TrimSuffix(localvideoFilePath, originalExt), originalExt)

	compressedVideoPath, err := compressVideo(localvideoFilePath, OutputFile)
	if err != nil {
		sendErrorResult("video", "compressed", req.VideoURL, fmt.Sprintf("Video compression failed: %v", err), req)
		return "", fmt.Errorf("video compression failed: %v", err)
	}

	// Upload to GCS
	wg.Add(1)
	go func(compressedPath, originalPath string) {
		defer wg.Done()

		// Upload to GCS only if enabled in config
		if config.EnableGCPUpload {
			if gcsUploader == nil {
				log.Printf("Error: GCS uploader not initialized")
				sendErrorResult("video", "compressed", req.VideoURL, "GCS uploader not initialized", req)
				return
			}

			path, err := uploadToGCS(compressedPath, "", req)
			if err != nil {
				log.Printf("Error uploading compressed video file to GCS: %v", err)
				sendErrorResult("video", "compressed", req.VideoURL, fmt.Sprintf("Upload failed: %v", err), req)
			} else {
				log.Printf("Compressed video uploaded successfully to GCS: %v", path)

				// Send success result
				result := MediaProcessResult{
					OriginalURL:    req.VideoURL,
					ProcessedURL:   path,
					MediaType:      "video",
					ProcessingType: "compressed",
					Success:        true,
				}
				sendProcessingResult(result, req)

				// Delete local files if enabled
				if config.DeleteLocalAfterUpload {
					// Always delete the compressed file
					log.Printf("Deleting compressed video: %s", compressedPath)
					os.Remove(compressedPath)

					// For the original stitched file, we can delete it since we have a dedicated copy for HLS/Dash
					if stitchedVideoPath != "" && stitchedVideoPath == originalPath {
						log.Printf("Deleting original stitched video: %s", originalPath)
						os.Remove(originalPath)
					} else if req.VideoURL != "" {
						// Delete the original downloaded file only if it's not a stitched file
						log.Printf("Deleting downloaded video: %s", originalPath)
						os.Remove(originalPath)
					}
				}
			}
		}
	}(compressedVideoPath, localvideoFilePath)
	log.Printf("Compressed Video: %s", compressedVideoPath)

	//Compress Image
	if req.ImgInputFile != "" {
		originalImgExt := filepath.Ext(localImgFilePath)
		OutputImgFile := fmt.Sprintf("%s_Compressed%s", strings.TrimSuffix(localImgFilePath, originalImgExt), originalImgExt)
		ImgQuality := 18

		compressedImgPath, err := compressImage(localImgFilePath, OutputImgFile, ImgQuality)
		if err != nil {
			sendErrorResult("image", "compressed", req.ImageURL, fmt.Sprintf("Image compression failed: %v", err), req)
			return "", fmt.Errorf("image compression failed: %v", err)
		}

		// Upload to GCS
		wg.Add(1)
		go func(compressedPath, originalPath string) {
			defer wg.Done()

			// Upload to GCS only if enabled in config
			if config.EnableGCPUpload {
				if gcsUploader == nil {
					log.Printf("Error: GCS uploader not initialized")
					sendErrorResult("image", "compressed", req.ImageURL, "GCS uploader not initialized", req)
					return
				}

				path, err := uploadToGCS(compressedPath, "", req)
				if err != nil {
					log.Printf("Error uploading compressed image file to GCS: %v", err)
					sendErrorResult("image", "compressed", req.ImageURL, fmt.Sprintf("Upload failed: %v", err), req)
				} else {
					log.Printf("Compressed image uploaded successfully to GCS: %v", path)

					// Send success result
					result := MediaProcessResult{
						OriginalURL:    req.ImageURL,
						ProcessedURL:   path,
						MediaType:      "image",
						ProcessingType: "compressed",
						Success:        true,
					}
					sendProcessingResult(result, req)

					// Delete local files if enabled
					if config.DeleteLocalAfterUpload {
						log.Printf("Deleting compressed image: %s", compressedPath)
						os.Remove(compressedPath)

						// Delete original downloaded image if it came from URL
						if req.ImageURL != "" {
							log.Printf("Deleting original image: %s", originalPath)
							os.Remove(originalPath)
						}
					}
				}
			}
		}(compressedImgPath, localImgFilePath)
		log.Printf("Compressed Image: %s", compressedImgPath)
	}
	return "Experience creation initiated successfully", nil
}

// Create Experience
func createExperienceHandler(w http.ResponseWriter, r *http.Request) {
	var req TranscodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON request", http.StatusBadRequest)
		return
	}

	message, err := processExperience(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(TranscodeResponse{Message: message})
}

func processTranscodeHlsDash(req TranscodeRequest) (string, error) {
	var localFilePath string
	var err error
	var alphaOrientation string // Renamed to avoid redeclaration

	if req.InputFile != "" {
		// Handle local file
		localFilePath = req.InputFile
		if _, err := os.Stat(localFilePath); os.IsNotExist(err) {
			sendErrorResult("video", "hls", req.VideoURL, "Input file not found", req)
			return "", fmt.Errorf("input file not found")
		}
	} else if req.VideoURL != "" {
		// Handle URL file
		localFilePath, err = download_Video(req.VideoURL)
		if err != nil {
			sendErrorResult("video", "hls", req.VideoURL, fmt.Sprintf("Failed to download video: %v", err), req)
			return "", fmt.Errorf("failed to download video")
		}
		// Register downloaded file with 1 reference (will be used for HLS/Dash generation)
		fileCleaner.RegisterFile(localFilePath, 1)
	} else {
		sendErrorResult("video", "hls", "", "Missing input_file or video_url parameter", req)
		return "", fmt.Errorf("missing input_file or video_url parameter")
	}

	// Check if the file is MOV format and extract alpha channel if needed
	var originalMOVPath string
	if checkFormat(localFilePath, "mov") {
		log.Println("Detected MOV file. Extracting alpha channel...")
		originalMOVPath = localFilePath
		localFilePath, alphaOrientation, err = processAlphaExtraction(localFilePath)
		if err != nil {
			sendErrorResult("video", "alpha", req.VideoURL, fmt.Sprintf("Alpha extraction failed: %v", err), req)
			return "", fmt.Errorf("alpha extraction failed: %v", err)
		}
		// Register the alpha-extracted file
		fileCleaner.RegisterFile(localFilePath, 1)
	}

	// Process the video asynchronously
	go func(orientation string) {
		err := generateHlsDash(localFilePath)
		if err != nil {
			log.Printf("Error processing video: %v", err)
			sendErrorResult("video", "hls", req.VideoURL, fmt.Sprintf("HLS/Dash generation failed: %v", err), req)
			return
		}

		if config.EnableGCPUpload {
			if gcsUploader == nil {
				log.Printf("Error: GCS uploader not initialized")
				sendErrorResult("video", "hls", req.VideoURL, "GCS uploader not initialized", req)
				return
			}

			baseName := strings.TrimSuffix(filepath.Base(localFilePath), filepath.Ext(localFilePath))
			outputDir := baseName
			log.Printf("Uploading to GCS: %s", outputDir)
			path, err := uploadToGCS(outputDir, "", req)
			if err != nil {
				log.Printf("Error uploading to GCS: %v", err)
				sendErrorResult("video", "hls", req.VideoURL, fmt.Sprintf("HLS/Dash upload failed: %v", err), req)
			} else {
				var hasAlpha bool
				log.Printf("Uploaded successfully to GCS: %v", path)

				// Construct the specific URLs for HLS and DASH files
				hlsFilePath := filepath.Join("transcoded", "h264_master.m3u8")
				dashFilePath := filepath.Join("transcoded", "h264_master.mpd")

				// Properly construct the full URLs
				hlsURL := fmt.Sprintf("%s/%s", path, hlsFilePath)
				dashURL := fmt.Sprintf("%s/%s", path, dashFilePath)

				log.Printf("HLS stream available at: %s", hlsURL)
				log.Printf("DASH stream available at: %s", dashURL)

				if orientation != "" {
					hasAlpha = true
				} else {
					hasAlpha = false
				}

				// Send success result
				result := MediaProcessResult{
					OriginalURL:    req.VideoURL,
					ProcessedURL:   path,
					HlsURL:         hlsURL,  // Add specific HLS URL
					DashURL:        dashURL, // Add specific DASH URL
					MediaType:      "video",
					ProcessingType: "hls",
					Orientation:    orientation,
					HasAlpha:       hasAlpha, // Always true for alpha-processed videos
					Success:        true,
				}
				sendProcessingResult(result, req)

				// Release reference to the processed file
				fileCleaner.ReleaseFile(localFilePath)

				// If we processed a MOV file, release the original MOV as well
				if originalMOVPath != "" && req.VideoURL != "" {
					fileCleaner.ReleaseFile(originalMOVPath)
				}
			}
		}
	}(alphaOrientation)

	return "Transcoding started", nil
}

// Handler for video to HLS and Dash transcoding
func generateHlsDashHandler(w http.ResponseWriter, r *http.Request) {
	var req TranscodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON request", http.StatusBadRequest)
		return
	}

	message, err := processTranscodeHlsDash(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(TranscodeResponse{Message: message})
}

func processalpha(req TranscodeRequest) (string, error) {
	var localFilePath string
	var err error

	if req.InputFile != "" {
		// Handle local file
		localFilePath = req.InputFile
		if _, err := os.Stat(localFilePath); os.IsNotExist(err) {
			sendErrorResult("video", "alpha", req.VideoURL, "Input file not found", req)
			return "", fmt.Errorf("input file not found")
		}
	} else if req.VideoURL != "" {
		// Handle URL file
		localFilePath, err = download_Video(req.VideoURL)
		if err != nil {
			sendErrorResult("video", "alpha", req.VideoURL, fmt.Sprintf("Failed to download video: %v", err), req)
			return "", fmt.Errorf("failed to download video: %v", err)
		}
		// Register downloaded file with 1 reference (will be used for alpha extraction)
		fileCleaner.RegisterFile(localFilePath, 1)
	} else {
		sendErrorResult("video", "alpha", "", "Missing input_file or video_url parameter", req)
		return "", fmt.Errorf("missing input_file or video_url parameter")
	}

	// Check if the file is MOV format and extract alpha channel if needed
	var originalMOVPath string
	var orientation string

	if checkFormat(localFilePath, "mov") {
		log.Println("Detected MOV file. Extracting alpha channel...")
		originalMOVPath = localFilePath
		localFilePath, orientation, err = processAlphaExtraction(localFilePath)
		if err != nil {
			sendErrorResult("video", "alpha", req.VideoURL, fmt.Sprintf("Alpha extraction failed: %v", err), req)
			return "", fmt.Errorf("alpha extraction failed: %v", err)
		}

		// Register the alpha-extracted file
		fileCleaner.RegisterFile(localFilePath, 1)
	}

	// Upload to GCS
	go func(orientation string) {
		// Upload to GCS only if enabled in config
		if config.EnableGCPUpload {
			if gcsUploader == nil {
				log.Printf("Error: GCS uploader not initialized")
				sendErrorResult("video", "alpha", req.VideoURL, "GCS uploader not initialized", req)
				return
			}

			path, err := uploadToGCS(localFilePath, "", req)
			if err != nil {
				log.Printf("Error uploading alpha-processed file to GCS: %v", err)
				sendErrorResult("video", "alpha", req.VideoURL, fmt.Sprintf("Upload failed: %v", err), req)
			} else {
				log.Printf("Alpha-processed file uploaded successfully to GCS: %v", path)

				// Send success result
				result := MediaProcessResult{
					OriginalURL:    req.VideoURL,
					ProcessedURL:   path,
					MediaType:      "video",
					ProcessingType: "alpha",
					Orientation:    orientation,
					HasAlpha:       true, // Always true for alpha-processed videos
					Success:        true,
				}
				sendProcessingResult(result, req)

				// Release reference to the processed file
				fileCleaner.ReleaseFile(localFilePath)

				// If we processed a MOV file, release the original MOV as well
				if originalMOVPath != "" && req.VideoURL != "" {
					fileCleaner.ReleaseFile(originalMOVPath)
				}
			}
		}
	}(orientation)

	return "Alpha extraction completed", nil
}

func generatealphaHandler(w http.ResponseWriter, r *http.Request) {
	var req TranscodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON request", http.StatusBadRequest)
		return
	}

	message, err := processalpha(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(TranscodeResponse{Message: message})
}

func processimagecompression(req TranscodeRequest) (string, error) {
	var localFilePath string
	var err error

	if req.ImgInputFile != "" {
		// Handle local file
		localFilePath = req.ImgInputFile
		if _, err := os.Stat(localFilePath); os.IsNotExist(err) {
			sendErrorResult("image", "compressed", req.ImageURL, "Input file not found", req)
			return "", fmt.Errorf("input file not found")
		}
	} else if req.ImageURL != "" {
		// Handle URL file
		localFilePath, err = download_Video(req.ImageURL)
		if err != nil {
			sendErrorResult("image", "compressed", req.ImageURL, fmt.Sprintf("Failed to download image: %v", err), req)
			return "", fmt.Errorf("failed to download image")
		}
		// Register downloaded file with 1 reference
		fileCleaner.RegisterFile(localFilePath, 1)
	} else {
		sendErrorResult("image", "compressed", "", "Missing input_file or image_url parameter", req)
		return "", fmt.Errorf("missing input_file or image_url parameter")
	}

	originalExt := filepath.Ext(localFilePath)
	OutputFile := fmt.Sprintf("%s_Compressed%s", strings.TrimSuffix(localFilePath, originalExt), originalExt)
	ImgQuality := 18

	compressedFilePath, err := compressImage(localFilePath, OutputFile, ImgQuality)
	if err != nil {
		sendErrorResult("image", "compressed", req.ImageURL, fmt.Sprintf("Image compression failed: %v", err), req)
		return "", fmt.Errorf("image compression failed: %v", err)
	}
	// Register compressed file with 1 reference
	fileCleaner.RegisterFile(compressedFilePath, 1)

	// Upload to GCS
	var resultPath string

	// Upload to GCS only if enabled in config
	if config.EnableGCPUpload {
		if gcsUploader == nil {
			sendErrorResult("image", "compressed", req.ImageURL, "GCS uploader not initialized", req)
			return "", fmt.Errorf("GCS uploader not initialized")
		}

		uploadedPath, err := uploadToGCS(compressedFilePath, "", req)
		if err != nil {
			sendErrorResult("image", "compressed", req.ImageURL, fmt.Sprintf("Upload failed: %v", err), req)
			return "", fmt.Errorf("error uploading compressed image file to GCS: %v", err)
		}

		// Send success result
		result := MediaProcessResult{
			OriginalURL:    req.ImageURL,
			ProcessedURL:   uploadedPath,
			MediaType:      "image",
			ProcessingType: "compressed",
			Success:        true,
		}
		sendProcessingResult(result, req)

		// Release reference to compressed file
		fileCleaner.ReleaseFile(compressedFilePath)

		// Release reference to original file if it was downloaded
		if req.ImageURL != "" {
			fileCleaner.ReleaseFile(localFilePath)
		}
		resultPath = "Image compression successful | Path: " + uploadedPath

	}

	// return "Image compression successful", nil
	return resultPath, nil
}

// Compress Image
func compressImageHandler(w http.ResponseWriter, r *http.Request) {
	var req TranscodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON request", http.StatusBadRequest)
		return
	}

	message, err := processimagecompression(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(TranscodeResponse{Message: message})
}

func processvideocompression(req TranscodeRequest) (string, error) {
	var localFilePath string
	var err error

	if req.InputFile != "" {
		// Handle local file
		localFilePath = req.InputFile
		if _, err := os.Stat(localFilePath); os.IsNotExist(err) {
			sendErrorResult("video", "compressed", req.VideoURL, "Input file not found", req)
			return "", fmt.Errorf("input file not found")
		}
	} else if req.VideoURL != "" {
		// Handle URL file
		localFilePath, err = download_Video(req.VideoURL)
		if err != nil {
			sendErrorResult("video", "compressed", req.VideoURL, fmt.Sprintf("Failed to download video: %v", err), req)
			return "", fmt.Errorf("failed to download video")
		}
		// Register downloaded file with 1 reference
		fileCleaner.RegisterFile(localFilePath, 1)
	} else {
		sendErrorResult("video", "compressed", "", "Missing input_file or video_url parameter", req)
		return "", fmt.Errorf("missing input_file or video_url parameter")
	}

	originalExt := filepath.Ext(localFilePath)
	OutputFile := fmt.Sprintf("%s_Compressed%s", strings.TrimSuffix(localFilePath, originalExt), originalExt)

	compressedFilePath, err := compressVideo(localFilePath, OutputFile)
	if err != nil {
		sendErrorResult("video", "compressed", req.VideoURL, fmt.Sprintf("Video compression failed: %v", err), req)
		return "", fmt.Errorf("video compression failed: %v", err)
	}
	// Register compressed file with 1 reference
	fileCleaner.RegisterFile(compressedFilePath, 1)

	// Upload to GCS
	go func() {
		// Upload to GCS only if enabled in config
		if config.EnableGCPUpload {
			if gcsUploader == nil {
				log.Printf("Error: GCS uploader not initialized")
				sendErrorResult("video", "compressed", req.VideoURL, "GCS uploader not initialized", req)
				return
			}

			path, err := uploadToGCS(compressedFilePath, "", req)
			if err != nil {
				log.Printf("Error uploading compressed video file to GCS: %v", err)
				sendErrorResult("video", "compressed", req.VideoURL, fmt.Sprintf("Upload failed: %v", err), req)
			} else {
				log.Printf("Compressed video uploaded successfully to GCS: %v", path)

				// Send success result
				result := MediaProcessResult{
					OriginalURL:    req.VideoURL,
					ProcessedURL:   path,
					MediaType:      "video",
					ProcessingType: "compressed",
					Success:        true,
				}
				sendProcessingResult(result, req)

				// Release reference to compressed file
				fileCleaner.ReleaseFile(compressedFilePath)

				// Release reference to original file if it was downloaded
				if req.VideoURL != "" {
					fileCleaner.ReleaseFile(localFilePath)
				}
			}
		}
	}()
	return "Video compression successful", nil
}

// Compress Video
func compressVideoHandler(w http.ResponseWriter, r *http.Request) {
	var req TranscodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON request", http.StatusBadRequest)
		return
	}

	message, err := processvideocompression(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(TranscodeResponse{Message: message})
}

// Generate signed URL for uploading to GCS
func generateSignedURLHandler(w http.ResponseWriter, r *http.Request) {
	var req SignedURLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON request", http.StatusBadRequest)
		return
	}

	// Set default expiration time if not specified
	if req.ExpirationMinutes <= 0 {
		req.ExpirationMinutes = 15 // 15 minutes by default
	}

	// Set default content type if not specified
	if req.ContentType == "" {
		req.ContentType = "application/octet-stream"
	}

	// Generate signed URL
	signedURL, err := gcsUploader.GenerateSignedURL(req.ObjectName, req.ContentType, req.ExpirationMinutes, config.GCPServiceAccountKeyPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to generate signed URL: %v", err), http.StatusInternalServerError)
		return
	}

	// Return the signed URL
	json.NewEncoder(w).Encode(SignedURLResponse{URL: signedURL})
}

// Generate multiple signed URLs for batch uploads
func generateBatchSignedURLsHandler(w http.ResponseWriter, r *http.Request) {
	// Set CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	var req BatchSignedURLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON request", http.StatusBadRequest)
		return
	}

	// Set default expiration time if not specified
	if req.ExpirationMinutes <= 0 {
		req.ExpirationMinutes = 15 // 15 minutes by default
	}

	// Set default content type if not specified
	if req.ContentType == "" {
		req.ContentType = "application/octet-stream"
	}

	// Generate batch signed URLs
	signedURLs, err := gcsUploader.GenerateBatchSignedURLs(req.ObjectNames, req.ContentType, req.ExpirationMinutes, config.GCPServiceAccountKeyPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to generate batch signed URLs: %v", err), http.StatusInternalServerError)
		return
	}

	// Return the signed URLs
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(BatchSignedURLResponse{URLs: signedURLs})
}

func processvideostitching(req TranscodeRequest) (string, error) {
	var localFilePath string
	var localAlphaFilePath string
	var err error

	if req.InputFile != "" {
		// Handle local file
		localFilePath = req.InputFile
		if _, err := os.Stat(localFilePath); os.IsNotExist(err) {
			sendErrorResult("video", "stitched", req.VideoURL, "Input file not found", req)
			return "", fmt.Errorf("input file not found")
		}
	} else if req.VideoURL != "" {
		// Handle URL file
		localFilePath, err = download_Video(req.VideoURL)
		if err != nil {
			sendErrorResult("video", "stitched", req.VideoURL, fmt.Sprintf("Failed to download video: %v", err), req)
			return "", fmt.Errorf("failed to download video")
		}
		// Register downloaded file with 1 reference (will be used for stitching)
		fileCleaner.RegisterFile(localFilePath, 1)
	} else {
		sendErrorResult("video", "stitched", "", "Missing input_file or video_url parameter", req)
		return "", fmt.Errorf("missing input_file or video_url parameter")
	}

	if req.AlphaInputFile != "" {
		// Handle local file
		localAlphaFilePath = req.AlphaInputFile
		if _, err := os.Stat(localAlphaFilePath); os.IsNotExist(err) {
			sendErrorResult("video", "stitched", req.AlphaVideoURL, "Alpha input file not found", req)
			return "", fmt.Errorf("input file not found")
		}
	} else if req.AlphaVideoURL != "" {
		// Handle URL file
		localAlphaFilePath, err = download_Video(req.AlphaVideoURL)
		if err != nil {
			sendErrorResult("video", "stitched", req.AlphaVideoURL, fmt.Sprintf("Failed to download alpha video: %v", err), req)
			return "", fmt.Errorf("failed to download video")
		}
		// Register downloaded file with 1 reference (will be used for stitching)
		fileCleaner.RegisterFile(localAlphaFilePath, 1)
	} else {
		sendErrorResult("video", "stitched", "", "Missing alphainput_file or alphavideo_url parameter", req)
		return "", fmt.Errorf("missing alphainput_file or alphavideo_url parameter")
	}

	originalExt := filepath.Ext(localFilePath)
	OutputFile := fmt.Sprintf("%s_stitched%s", strings.TrimSuffix(localFilePath, originalExt), originalExt)

	stitchedFilePath, orientation, err := stitchVideos(localFilePath, localAlphaFilePath, OutputFile)
	if err != nil {
		sendErrorResult("video", "stitched", req.VideoURL, fmt.Sprintf("Video stitching failed: %v", err), req)
		return "", fmt.Errorf("video stitching failed: %v", err)
	}
	// Register stitched file with 1 reference
	fileCleaner.RegisterFile(stitchedFilePath, 1)

	// Upload to GCS
	go func(orientation string) {
		// Upload to GCS only if enabled in config
		if config.EnableGCPUpload {
			if gcsUploader == nil {
				log.Printf("Error: GCS uploader not initialized")
				sendErrorResult("video", "stitched", req.VideoURL, "GCS uploader not initialized", req)
				return
			}

			path, err := uploadToGCS(stitchedFilePath, "", req)
			if err != nil {
				log.Printf("Error uploading stitched video file to GCS: %v", err)
				sendErrorResult("video", "stitched", req.VideoURL, fmt.Sprintf("Upload failed: %v", err), req)
			} else {
				log.Printf("Stitched video uploaded successfully to GCS: %v", path)

				// Send success result
				result := MediaProcessResult{
					OriginalURL:    req.VideoURL,
					ProcessedURL:   path,
					MediaType:      "video",
					ProcessingType: "stitched",
					Orientation:    orientation,
					HasAlpha:       true, // Always true for stitched videos
					Success:        true,
				}
				sendProcessingResult(result, req)

				// Release reference to stitched file
				fileCleaner.ReleaseFile(stitchedFilePath)

				// Release reference to original files if they were downloaded
				if req.VideoURL != "" {
					fileCleaner.ReleaseFile(localFilePath)
				}
				if req.AlphaVideoURL != "" {
					fileCleaner.ReleaseFile(localAlphaFilePath)
				}
			}
		}
	}(orientation)
	return "Video stitching successful", nil
}

// Stitch Original and Alpha mp4 video based on orientation
func stitchVideosHandler(w http.ResponseWriter, r *http.Request) {
	var req TranscodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON request", http.StatusBadRequest)
		return
	}

	message, err := processvideostitching(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(TranscodeResponse{Message: message})
}

// Extract alpha channel from a MOV file
func processAlphaExtraction(localFilePath string) (string, string, error) {
	// Get video resolution
	width, height, err := getVideoResolution(localFilePath)
	if err != nil {
		return "", "", fmt.Errorf("failed to get video resolution: %v", err)
	}

	// Determine orientation: vstack if height > width, otherwise hstack
	orientation := "vstack"
	if height > width {
		orientation = "hstack"
	}

	// Process video to extract alpha channel
	alphaProcessedFile := fmt.Sprintf("%s_Final.mp4", strings.TrimSuffix(localFilePath, filepath.Ext(localFilePath)))
	err = generateAlpha(localFilePath, alphaProcessedFile, width, height, orientation)
	if err != nil {
		return "", "", fmt.Errorf("alpha extraction failed: %v", err)
	}

	return alphaProcessedFile, orientation, nil
}

// Download video from URL using yt-dlp
func download_Video(videoURL string) (string, error) {
	// Generate a unique filename using a timestamp or UUID
	uniqueID := time.Now().UnixNano() // Alternative: uuid.New().String()
	// filenamePattern := fmt.Sprintf("downloaded_video_%d.%%(ext)s", uniqueID)
	filenamePattern := fmt.Sprintf("%d.%%(ext)s", uniqueID)

	// Run yt-dlp command with the unique filename
	cmd := exec.Command("yt-dlp", "-f", "best", "-o", filenamePattern, videoURL)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return "", err
	}

	// Find the downloaded file
	// files, err := filepath.Glob(fmt.Sprintf("downloaded_video_%d.*", uniqueID))
	files, err := filepath.Glob(fmt.Sprintf("%d.*", uniqueID))
	if err != nil || len(files) == 0 {
		return "", fmt.Errorf("could not find downloaded video")
	}

	return files[0], nil
}

// Check if the input file is a MOV format using ffprobe
// func isMOVFormat(inputFile string) bool {
func checkFormat(filePath, expectedFormat string) bool {
	ext := strings.ToLower(filepath.Ext(filePath))   // Get file extension and convert to lowercase
	expectedFormat = strings.ToLower(expectedFormat) // Normalize expected extension

	// Remove the leading dot from expectedExt if present
	expectedFormat = strings.TrimPrefix(expectedFormat, ".")

	// Compare with actual extension (remove the dot before comparison)
	return strings.TrimPrefix(ext, ".") == expectedFormat
}

// Main video processing function
func generateHlsDash(inputFile string) error {
	// Get video resolution
	videoWidth, videoHeight, err := getVideoResolution(inputFile)
	if err != nil {
		return err
	}
	maxResLimit := max(videoWidth, videoHeight)

	// Create output directory
	baseName := strings.TrimSuffix(filepath.Base(inputFile), filepath.Ext(inputFile))
	outputDir := baseName
	if err := os.MkdirAll(outputDir, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create output directory: %v", err)
	}

	// Transcode video for each resolution
	var packagerInputs []string
	// for _, res := range resolutions {
	for _, res := range config.Resolutions {
		if res <= maxResLimit {
			bitrate := getBitrate(res)
			outputRes := fmt.Sprintf("%dp", res)
			outputFile := filepath.Join(outputDir, fmt.Sprintf("output_%s.mp4", outputRes))

			// Transcode video
			if err := runFFmpeg(inputFile, outputFile, res, bitrate); err != nil {
				return err
			}

			// Add to packager inputs
			packagerInputs = append(packagerInputs,
				fmt.Sprintf("in=%s,stream=video,output=%s,playlist_name=h264_%s.m3u8,iframe_playlist_name=h264_%s_iframe.m3u8",
					outputFile, filepath.Join(outputDir, fmt.Sprintf("transcoded/h264_%s.mp4", outputRes)), outputRes, outputRes))
		}
	}

	// Package HLS & DASH
	return packagerHlsDash(inputFile, outputDir, packagerInputs)
}

// Extract video resolution using ffprobe
func getVideoResolution(inputFile string) (int, int, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "csv=s=x:p=0", inputFile)
	output, err := cmd.Output()
	if err != nil {
		return 0, 0, fmt.Errorf("ffprobe failed: %v", err)
	}

	parts := strings.Split(strings.TrimSpace(string(output)), "x")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unexpected ffprobe output: %s", output)
	}

	width, _ := strconv.Atoi(parts[0])
	height, _ := strconv.Atoi(parts[1])
	return width, height, nil
}

// Run FFmpeg for video transcoding
func runFFmpeg(inputFile, outputFile string, res int, bitrate string) error {
	// "-vf", fmt.Sprintf("scale='floor(iw*%d/ih/16)*16':%d", res, res), "-c:v", "libx264",
	// First check if input file has audio
	hasAudio, err := checkIfFileHasAudio(inputFile)
	if err != nil {
		log.Printf("Warning: Could not determine if file has audio: %v", err)
		// Continue anyway, assume it might have audio
		hasAudio = true
	}

	// Base arguments for FFmpeg
	args := []string{
		"-i", inputFile,
		"-vf", fmt.Sprintf("scale='if(gte(iw,ih),%d,floor(iw*%d/ih/16)*16)':'if(gt(ih,iw),%d,floor(ih*%d/iw/16)*16)':flags=lanczos",
			res, res, res, res),
		"-c:v", "libx264",
		"-profile:v", "baseline",
		"-level:v", "3.0",
		"-x264-params", "scenecut=0:open_gop=0:min-keyint=72:keyint=72",
		"-minrate", bitrate,
		"-maxrate", bitrate,
		"-bufsize", bitrate,
		"-b:v", bitrate,
	}

	// Add audio arguments only if the input has audio
	if hasAudio {
		args = append(args, "-c:a", "copy")
	} else {
		// If no audio, tell FFmpeg to ignore audio streams
		args = append(args, "-an")
		log.Printf("Input file %s has no audio stream, skipping audio transcoding", inputFile)
	}

	// Add output file and overwrite flag
	args = append(args, "-y", outputFile)

	// Create the command with all arguments
	cmd := exec.Command("ffmpeg", args...)

	var stderr bytes.Buffer
	cmd.Stdout = os.Stdout
	cmd.Stderr = &stderr

	// Run the command
	if err := cmd.Run(); err != nil {
		log.Printf("FFmpeg error output: %s", stderr.String())
		return fmt.Errorf("ffmpeg failed: %v", err)
	}

	return nil
}

// 'if(gte(iw,ih),%s,floor(iw*%s/ih/16)*16)':'if(gt(ih,iw),%s,floor(ih*%s/iw/16)*16)'
// Compress Image
func compressImage(inputFile, outputFile string, quality int) (string, error) {

	maxSize := config.ImageSize

	// Build the ffmpeg command
	cmd := exec.Command(
		"ffmpeg",
		"-i", inputFile,
		"-vf", fmt.Sprintf("scale='if(gte(iw,ih),%d,-1):if(gt(ih,iw),%d,-1)'", maxSize, maxSize),
		"-q:v", "3", "-y",
		outputFile)

	// cmd := exec.Command("ffmpeg", "-i", inputFile, "-qscale:v", strconv.Itoa(quality), outputFile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return outputFile, nil
}

// Compress Video
func compressVideo(inputFile, outputFile string) (string, error) {
	compressSize := config.VideoSize
	cmd := exec.Command("ffmpeg",
		"-y", "-i", inputFile,
		"-preset", "veryfast", "-tune", "zerolatency", "-crf", strconv.Itoa(config.VideoCRF),
		"-codec:v", "libx264", "-codec:a", "aac", "-b:a", "128k",
		"-vf", fmt.Sprintf("scale='if(gt(iw,%d),if(gte(iw,ih),%d,-2),-2)':'if(gt(ih,%d),if(gt(ih,iw),%d,-2),-2)':flags=lanczos",
			compressSize, compressSize, compressSize, compressSize),
		"-movflags", "+faststart", "-y",
		outputFile)

	// cmd := exec.Command("ffmpeg", "-i", inputFile, "-c:v", "libx265", "-crf", strconv.Itoa(crf), "-c:a", "aac", "-b:a", "128k", outputFile)
	// cmd := exec.Command("ffmpeg", "-i", inputFile,
	// 	"-c:v", "libx265", "-crf", strconv.Itoa(crf), "-preset", "medium",
	// 	"-pix_fmt", "yuv420p", // Ensures compatibility
	// 	"-c:a", "aac", "-b:a", "128k",
	// 	outputFile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return outputFile, nil
}

// Stitch Video
func stitchVideos(originalFile, alphaFile, outputFile string) (string, string, error) {

	width, height, err := getVideoResolution(originalFile)
	if err != nil {
		return "", "", fmt.Errorf("failed to get video resolution: %v", err)
	}

	// Determine orientation: vstack if height > width, otherwise hstack
	orientation := "vstack"
	if height > width {
		orientation = "hstack"
	}

	cmd := exec.Command("ffmpeg", "-i", originalFile, "-i", alphaFile, "-filter_complex",
		fmt.Sprintf("[%s][%s]%s[out]", "0:v", "1:v", orientation),
		"-map", "[out]", "-map", "0:a?", "-c:v", "libx264", "-preset", "slow",
		"-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "192k", "-y", outputFile)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// return cmd.Run()
	err = cmd.Run()
	if err != nil {
		return "", "", err
	}
	return outputFile, orientation, nil
}

// Package HLS & DASH using Shaka Packager
func packagerHlsDash(inputFile, outputDir string, packagerInputs []string) error {
	// First check if input file has audio
	hasAudio, err := checkIfFileHasAudio(inputFile)
	if err != nil {
		log.Printf("Warning: Could not determine if file has audio: %v", err)
		// Continue anyway, assuming it might have audio
		hasAudio = true
	}

	// Only add audio packaging if the file actually has audio
	if hasAudio {
		audioOutput := filepath.Join(outputDir, "transcoded/audio.mp4")
		packagerInputs = append(packagerInputs,
			fmt.Sprintf("in=%s,stream=audio,output=%s,bandwidth=128000", inputFile, audioOutput))
	} else {
		log.Printf("Input file %s has no audio stream, skipping audio packaging", inputFile)
	}

	args := append(packagerInputs,
		"--segment_duration", "3",
		"--hls_master_playlist_output", filepath.Join(outputDir, "transcoded/h264_master.m3u8"),
		"--mpd_output", filepath.Join(outputDir, "transcoded/h264_master.mpd"),
	)

	log.Printf("Running packager with arguments: %v", args)

	// cmd := exec.Command("packager", args...)
	// cmd.Stdout = os.Stdout
	// cmd.Stderr = os.Stderr
	// return cmd.Run()
	cmd := exec.Command("packager", args...)
	var stderr bytes.Buffer
	cmd.Stdout = os.Stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		log.Printf("Packager error output: %s", stderr.String())
		return fmt.Errorf("packager failed: %v", err)
	}

	return nil
}

// checkIfFileHasAudio uses ffprobe to check if a file has an audio stream
func checkIfFileHasAudio(inputFile string) (bool, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "stream=codec_type",
		"-of", "csv=p=0",
		inputFile)

	output, err := cmd.Output()
	if err != nil {
		// If ffprobe returns an error, it might be because there's no audio stream
		return false, err
	}

	// If output contains "audio", file has an audio stream
	return strings.Contains(string(output), "audio"), nil
}

func generateAlpha(inputFile, outputFile string, width, height int, orientation string) error {
	// Ensure height is even
	if height%2 != 0 {
		height++
	}

	// Print the detected orientation
	fmt.Printf("Processing video with orientation: %s\n", orientation)

	ffmpegCmd := exec.Command("ffmpeg", "-i", inputFile, "-filter_complex",
		fmt.Sprintf(`
		[0:v]split=2[m][a];
		[m]scale=%dx%d[m_scaled];
		[a]alphaextract,format=gray,scale=%dx%d[alpha];
		[m_scaled][alpha]%s[out]`,
			width, height, width, height, orientation),
		"-map", "[out]", "-map", "0:a?", // Ensure audio is included if available
		"-c:v", "libx264", "-crf", strconv.Itoa(config.VideoCRF), "-preset", "slow",
		"-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "192k", "-y", outputFile)

	ffmpegCmd.Stdout = os.Stdout
	ffmpegCmd.Stderr = os.Stderr

	return ffmpegCmd.Run()
}

// Get bitrate based on resolution
func getBitrate(res int) string {
	switch {
	case res < 480:
		return "600k"
	case res < 720:
		return "1000k"
	case res < 1088:
		return "2500k"
	case res < 1440:
		return "4500k"
	case res < 2160:
		return "9000k"
	case res < 2880:
		return "20000k"
	case res < 5760:
		return "50000k"
	default:
		return "100000k"
	}
}

// Get max of two integers
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Instead of running the gcpupload.go as a separate process, use the package directly
func uploadToGCS(filePath, subFolder string, request TranscodeRequest) (string, error) {
	// Check if uploader is initialized
	if gcsUploader == nil {
		return "", fmt.Errorf("GCS uploader not initialized")
	}

	cleanedFilePath := strings.TrimSpace(filePath) // Remove leading and trailing spaces
	cleanedSubFolder := strings.TrimSpace(subFolder)

	// Construct the full bucket path
	fullBucketPath := config.GCPBucketPath

	// Add organization ID to the path if available
	if request.OrganizationID != "" {
		fullBucketPath = filepath.Join(fullBucketPath, "organizations", request.OrganizationID)
	}

	// Add subfolder if provided
	if cleanedSubFolder != "" {
		fullBucketPath = filepath.Join(fullBucketPath, cleanedSubFolder)
	}

	// Ensure the path doesn't have double slashes
	fullBucketPath = strings.Replace(fullBucketPath, "//", "/", -1)

	// Remove leading slash if present to prevent double slash in URLs
	fullBucketPath = strings.TrimPrefix(fullBucketPath, "/")

	// Process file or directory path to get the object name
	var uploadPath string
	fileInfo, err := os.Stat(cleanedFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to access file/folder: %v", err)
	}

	// Different handling for files and directories
	if fileInfo.IsDir() {
		// For directories, we need to use the UploadFolder method
		uploadPath, err = gcsUploader.UploadFolder(cleanedFilePath, fullBucketPath)
		if err != nil {
			return "", fmt.Errorf("failed to upload folder: %v", err)
		}

		// Delete local directory after successful upload if GCP upload is enabled
		if config.EnableGCPUpload && config.DeleteLocalAfterUpload {
			log.Printf("Deleting local directory after successful upload: %s", cleanedFilePath)
			if err := os.RemoveAll(cleanedFilePath); err != nil {
				log.Printf("Warning: Failed to delete local directory: %v", err)
			}
		}
	} else {
		// For individual files, construct the object name and use UploadFile
		objectName := filepath.Join(fullBucketPath, filepath.Base(cleanedFilePath))
		uploadPath, err = gcsUploader.UploadFile(cleanedFilePath, objectName)
		if err != nil {
			return "", fmt.Errorf("failed to upload file: %v", err)
		}

		// Delete local file after successful upload if enabled, but only if not preserved
		if config.EnableGCPUpload && config.DeleteLocalAfterUpload {
			log.Printf("Upload complete for file: %s", cleanedFilePath)
			fileCleaner.ReleaseFile(cleanedFilePath)
		}
	}

	// Log the complete GCS path where the asset was uploaded
	httpPath := fmt.Sprintf("https://storage.googleapis.com/%s/%s",
		config.GCPBucketName,
		filepath.Join(fullBucketPath, filepath.Base(cleanedFilePath)))

	log.Printf("Asset uploaded to GCS path: %s", uploadPath)
	log.Printf("Asset accessible at: %s", httpPath)

	return httpPath, nil
}

// Worker function to process messages
func worker(nc *nats.Conn, subject, queueGroup string, processFunc func(TranscodeRequest) (string, error), wg *sync.WaitGroup) {
	defer wg.Done()

	_, err := nc.QueueSubscribe(subject, queueGroup, func(msg *nats.Msg) {
		var req TranscodeRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			log.Println("Invalid JSON in NATS message:", err)
			return
		}

		log.Printf("Worker processing task on subject: %s", subject)
		_, err := processFunc(req) // Ignore returned string
		if err != nil {
			log.Printf("Error processing task on subject %s: %v", subject, err)
		}
		log.Printf("Successfully processed task on subject: %s", subject)
	})
	if err != nil {
		log.Fatalf("Failed to subscribe to %s: %v", subject, err)
	}
	log.Printf("Worker subscribed to subject: %s", subject)
}

// CORS middleware to handle preflight requests and add required headers
func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, Authorization")

		// Handle preflight requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Call the next handler
		next(w, r)
	}
}

// Start HTTP server
func main() {
	// Load configuration
	err := loadConfig("config.json")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Print loaded config (for debugging)
	fmt.Printf("Config: %+v\n", config)

	// Initialize the GCS uploader if uploads are enabled
	if config.EnableGCPUpload {
		log.Println("Initializing GCS uploader...")
		var err error

		gcsUploader, err = gcpupload.NewGCSUploader(config.GCPBucketName)
		if err != nil {
			log.Printf("FATAL: Failed to initialize GCS uploader: %v", err)
			// Don't fatally exit, just disable GCS uploads by setting the flag to false
			config.EnableGCPUpload = false
		} else {
			log.Println("GCS uploader initialized successfully with bucket:", config.GCPBucketName)
			defer gcsUploader.Close()
		}
	} else {
		log.Println("GCS uploads are disabled in config")
	}

	// // Process function mapping
	// processFuncMap := map[string]func(TranscodeRequest) (string, error){
	// 	"createexperience": processExperience,
	// 	"transcodehlsdash": processTranscodeHlsDash,
	// 	"generatealpha":    processalpha,
	// 	"compressimage":    processimagecompression,
	// 	"compressvideo":    processvideocompression,
	// 	"stitchvideos":     processvideostitching,
	// }

	// Connect to NATS
	nc, err := nats.Connect(nats.DefaultURL)
	if err != nil {
		log.Fatalf("Failed to connect to NATS: %v", err)
	}
	defer nc.Close()

	// Define queue group name
	queueGroup := "task_queue"

	// Use a wait group to manage worker lifetimes
	var wg sync.WaitGroup

	// Launch multiple workers per task type
	for i := 0; i < config.NumberOfWorkers; i++ {
		wg.Add(6) // 6 task types, so each worker subscribes to all

		go worker(nc, "createexperience", queueGroup, processExperience, &wg)
		go worker(nc, "transcodehlsdash", queueGroup, processTranscodeHlsDash, &wg)
		go worker(nc, "generatealpha", queueGroup, processalpha, &wg)
		go worker(nc, "compressimage", queueGroup, processimagecompression, &wg)
		go worker(nc, "compressvideo", queueGroup, processvideocompression, &wg)
		go worker(nc, "stitchvideos", queueGroup, processvideostitching, &wg)
	}

	// Initialize HTTP server with CORS support
	mux := http.NewServeMux()

	// Register all HTTP endpoints with CORS support
	mux.HandleFunc("/createexperience", corsMiddleware(createExperienceHandler))
	mux.HandleFunc("/transcodehlsdash", corsMiddleware(generateHlsDashHandler))
	mux.HandleFunc("/generatealpha", corsMiddleware(generatealphaHandler))
	mux.HandleFunc("/compressimage", corsMiddleware(compressImageHandler))
	mux.HandleFunc("/compressvideo", corsMiddleware(compressVideoHandler))
	mux.HandleFunc("/stitchvideos", corsMiddleware(stitchVideosHandler))
	mux.HandleFunc("/generatesignedurl", corsMiddleware(generateSignedURLHandler))
	mux.HandleFunc("/generatebatchsignedurls", corsMiddleware(generateBatchSignedURLsHandler))

	// Add health check endpoint
	mux.HandleFunc("/health", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"version": "1.0.0",
		})
	}))

	// Serve static files and test pages
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))
	mux.HandleFunc("/upload", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./static/upload.html")
	}))
	mux.HandleFunc("/bulk-upload", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./static/bulk-upload.html")
	}))

	// Start the HTTP server
	server := &http.Server{
		Addr:    ":8082",
		Handler: mux,
	}

	// Start the HTTP server in a goroutine
	go func() {
		log.Println("Server running on port 8082...")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	// Set up graceful shutdown for the server
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)

	// Wait for termination signal
	<-signalChan
	log.Println("Received shutdown signal, gracefully shutting down...")

	// Create a context with a timeout for shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Attempt to shutdown the server gracefully
	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Server shutdown failed: %v", err)
	}

	log.Println("Server gracefully shut down")

	// // Start the HTTP server concurrently
	// go func() {
	// 	http.HandleFunc("/createexperience", createExperienceHandler)
	// 	http.HandleFunc("/transcodehlsdash", generateHlsDashHandler)
	// 	http.HandleFunc("/generatealpha", generatealphaHandler)
	// 	http.HandleFunc("/compressimage", compressImageHandler)
	// 	http.HandleFunc("/compressvideo", compressVideoHandler)
	// 	http.HandleFunc("/stitchvideos", stitchVideosHandler)

	// 	// Add the new signed URL handlers
	// 	http.HandleFunc("/generatesignedurl", corsMiddleware(generateSignedURLHandler))
	// 	http.HandleFunc("/generatebatchsignedurls", corsMiddleware(generateBatchSignedURLsHandler))

	// 	// Serve static files
	// 	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))

	// 	// Serve the upload test page
	// 	http.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
	// 		http.ServeFile(w, r, "./static/upload.html")
	// 	})

	// 	http.HandleFunc("/bulk-upload", func(w http.ResponseWriter, r *http.Request) {
	// 		http.ServeFile(w, r, "./static/bulk-upload.html")
	// 	})

	// 	log.Println("Server running on port 8082...")
	// 	log.Fatal(http.ListenAndServe(":8082", nil))
	// }()

	// // Wait for all workers to complete (this never happens because it's a long-running service)
	// wg.Wait()

	// // Keep the program running
	// select {}

}

// Authenticate to google cloud
// gcloud auth application-default login

// curl -X POST http://localhost:8080/createexperience \
//      -H "Content-Type: application/json" \
//      -d '{"imageinput_file": "Gulf_News.jpg", "input_file": "Gulf_News.mp4", "alphainput_file": "Gulf_News_Alpha.mp4"}'

// curl -X POST http://localhost:8080/transcodehlsdash \
//      -H "Content-Type: application/json" \
//      -d '{"input_file": "Gulf_News_stitch.mp4"}'

// curl -X POST http://localhost:8080/transcodehlsdash \
//      -H "Content-Type: application/json" \
//      -d '{"video_url": "https://storage.googleapis.com/zingcam/original/videos/ahg90z0rv3iu2j4phhg1ncsm.mp4"}'

// curl -X POST http://localhost:8080/transcodehlsdash \
//      -H "Content-Type: application/json" \
//      -d '{"input_file": "Alcohol.mov"}'

// curl -X POST http://localhost:8080/transcodehlsdash \
//      -H "Content-Type: application/json" \
//      -d '{"video_url": "https://storage.googleapis.com/zingcam-dev/dev/packager/Shaka_Test/Jewellery.mov"}'

// curl -X POST http://localhost:8080/generatealpha \
//      -H "Content-Type: application/json" \
//      -d '{"video_url": "https://storage.googleapis.com/zingcam-dev/dev/packager/Shaka_Test/Jewellery.mov"}'

// curl -X POST http://localhost:8080/generatealpha \
//      -H "Content-Type: application/json" \
//      -d '{"input_file": "Alcohol.mov"}'

// curl -X POST http://localhost:8080/compressimage \
//      -H "Content-Type: application/json" \
//      -d '{"image_url": "https://storage.googleapis.com/zingcam/original/images/c82ssfbqqrk5qt5kx7j7r1zj.jpg"}'

// curl -X POST http://localhost:8080/compressimage \
//      -H "Content-Type: application/json" \
//      -d '{"imageinput_file": "FreedomOil.jpg"}'

// curl -X POST http://localhost:8080/compressvideo \
//      -H "Content-Type: application/json" \
//      -d '{"video_url": "https://storage.googleapis.com/zingcam/original/videos/y1ioa5ybwdjp73irih8v7mvs.mp4"}'

// curl -X POST http://localhost:8080/compressvideo \
//      -H "Content-Type: application/json" \
//      -d '{"input_file": "royalstag.mp4"}'

// curl -X POST http://localhost:8080/stitchvideos \
//      -H "Content-Type: application/json" \
//      -d '{"video_url": "https://storage.googleapis.com/zingcam/original/videos/jhnrifj6wtrq1btcqb5yk7hm.mp4", "alphavideo_url": "https://storage.googleapis.com/zingcam/original/videos/t5eamnco2vej6d1zqtwkuqku.mp4"}'

// curl -X POST http://localhost:8080/stitchvideos \
//      -H "Content-Type: application/json" \
//      -d '{"input_file": "Gulf_News.mp4", "alphainput_file": "Gulf_News_Alpha.mp4"}'
