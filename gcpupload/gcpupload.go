package gcpupload

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

// gcloud auth application-default login // This grants access to your GCP bucket when using Go code
// GCSUploader handles signed URL generation and file upload
type GCSUploader struct {
	BucketName string
	client     *storage.Client
}

// NewGCSUploader initializes a new GCSUploader
func NewGCSUploader(bucketName string) (*GCSUploader, error) {
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage client: %v", err)
	}

	return &GCSUploader{BucketName: bucketName, client: client}, nil
}

// NewGCSUploaderWithCredentials initializes a new GCSUploader with specific credentials
func NewGCSUploaderWithCredentials(bucketName string, credentialsPath string) (*GCSUploader, error) {
	ctx := context.Background()
	client, err := storage.NewClient(ctx, option.WithCredentialsFile(credentialsPath))
	if err != nil {
		return nil, fmt.Errorf("failed to create storage client with credentials: %v", err)
	}

	return &GCSUploader{BucketName: bucketName, client: client}, nil
}

// GenerateSignedURL generates a signed URL for uploading a file to GCS
func (uploader *GCSUploader) GenerateSignedURL(objectName string, contentType string, expirationMinutes int, keyFilePath string) (string, error) {
	// Read the key file from the path provided in config
	keyBytes, err := os.ReadFile(keyFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to read service account key: %v", err)
	}

	// Parse the key file to extract the service account email and private key
	var keyJSON struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal(keyBytes, &keyJSON); err != nil {
		return "", fmt.Errorf("failed to parse service account key: %v", err)
	}

	// ctx := context.Background()

	// Create the signed URL options
	opts := &storage.SignedURLOptions{
		GoogleAccessID: keyJSON.ClientEmail,
		PrivateKey:     []byte(keyJSON.PrivateKey),
		Method:         "PUT", // or "GET" depending on your needs
		Expires:        time.Now().Add(time.Duration(expirationMinutes) * time.Minute),
		ContentType:    contentType,
		Scheme:         storage.SigningSchemeV4,
	}

	// Generate the signed URL
	url, err := storage.SignedURL(uploader.BucketName, objectName, opts)
	if err != nil {
		return "", fmt.Errorf("failed to generate signed URL: %v", err)
	}

	return url, nil
}

// UploadFileWithSignedURL uploads a file using a pre-generated signed URL
func UploadFileWithSignedURL(filePath string, signedURL string, contentType string) error {
	// Open the file
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %v", err)
	}
	defer file.Close()

	// Get file size
	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to get file info: %v", err)
	}
	fileSize := fileInfo.Size()

	// Create a new HTTP request
	req, err := http.NewRequest(http.MethodPut, signedURL, file)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %v", err)
	}

	// Set headers
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Content-Length", fmt.Sprintf("%d", fileSize))

	// Send the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to upload file: %v", err)
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload failed with status %s: %s", resp.Status, string(respBody))
	}

	log.Printf("File uploaded successfully using signed URL: %s", filePath)
	return nil
}

// UploadFile uploads a single file to GCS
func (uploader *GCSUploader) UploadFile(filePath string, objectName string) (string, error) {
	ctx := context.Background()
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %v", err)
	}
	defer file.Close()

	wc := uploader.client.Bucket(uploader.BucketName).Object(objectName).NewWriter(ctx)
	if _, err := io.Copy(wc, file); err != nil {
		return "", fmt.Errorf("failed to copy file to GCS: %v", err)
	}

	if err := wc.Close(); err != nil {
		return "", fmt.Errorf("failed to close writer: %v", err)
	}

	uploadPath := fmt.Sprintf("gs://%s/%s", uploader.BucketName, objectName)
	log.Printf("File uploaded: %s -> %s", filePath, uploadPath)
	return uploadPath, nil
}

// UploadFolder uploads all files in a folder to GCS and returns the base path
func (uploader *GCSUploader) UploadFolder(folderPath string, destinationPrefix string) (string, error) {
	// Get folder name
	folderName := filepath.Base(folderPath)
	basePath := filepath.Join(destinationPrefix, folderName)

	err := filepath.Walk(folderPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() {
			// Keep relative structure
			relPath, _ := filepath.Rel(folderPath, path)

			// Create correct GCS object path
			objectPath := filepath.Join(destinationPrefix, folderName, relPath)
			log.Printf("Uploading %s to gs://%s/%s", path, uploader.BucketName, objectPath)

			_, err := uploader.UploadFile(path, objectPath)
			return err
		}

		return nil
	})

	if err != nil {
		return "", err
	}

	return fmt.Sprintf("gs://%s/%s", uploader.BucketName, basePath), nil
}

// GenerateBatchSignedURLs generates multiple signed URLs for a list of object names
func (uploader *GCSUploader) GenerateBatchSignedURLs(objectNames []string, contentType string, expirationMinutes int, keyFilePath string) (map[string]string, error) {
	urls := make(map[string]string)

	for _, objectName := range objectNames {
		genericContentType := "application/octet-stream"
		url, err := uploader.GenerateSignedURL(objectName, genericContentType, expirationMinutes, keyFilePath)
		if err != nil {
			return nil, fmt.Errorf("failed to generate signed URL for %s: %v", objectName, err)
		}
		urls[objectName] = url
	}

	return urls, nil
}

// Close closes the storage client
func (uploader *GCSUploader) Close() {
	if uploader.client != nil {
		uploader.client.Close()
	}
}
