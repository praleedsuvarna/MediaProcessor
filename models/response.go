package models

type ExperienceResult struct {
	OriginalPath     string  `json:"original_path"`
	CompressedPath   string  `json:"compressed_path"`
	GCSPath          string  `json:"gcs_path,omitempty"`
	CompressionRatio float64 `json:"compression_ratio,omitempty"`
	FileSize         int64   `json:"file_size,omitempty"`
}

type CommonResult struct {
	UploadPath string `json:"uploaded_path"`
}

// type CommonResult struct {
//     OriginalPath     string `json:"original_path"`
//     CompressedPath   string `json:"compressed_path"`
//     GCSPath          string `json:"gcs_path,omitempty"`
//     CompressionRatio float64 `json:"compression_ratio,omitempty"`
//     FileSize         int64   `json:"file_size,omitempty"`
// }
