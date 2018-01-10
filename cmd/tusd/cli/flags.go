package cli

import (
	"flag"
	"path/filepath"
)

var Flags struct {
	SyncEnabled  bool
	SyncAddr     string
	SyncUri      string
	SyncDataDir  string
	SyncInterval int64
	SyncType     int
	SyncId       int
	SyncSecure   bool

	HttpHost          string
	HttpPort          string
	MaxSize           int64
	UploadDir         string
	StoreSize         int64
	Basepath          string
	Timeout           int64
	S3Bucket          string
	S3Endpoint        string
	GCSBucket         string
	FileHooksDir      string
	HttpHooksEndpoint string
	HttpHooksRetry    int
	HttpHooksBackoff  int
	ShowVersion       bool
	ExposeMetrics     bool
	MetricsPath       string
	BehindProxy       bool

	FileHooksInstalled bool
	HttpHooksInstalled bool
}

func ParseFlags() {
	flag.StringVar(&Flags.SyncAddr, "sync-addr", "localhost:8080", "Host to connect/sync to")
	flag.StringVar(&Flags.SyncUri, "sync-uri", "/", "http uri")
	flag.StringVar(&Flags.SyncDataDir, "sync-data-dir", "", "dir where the sync data resides in")
	flag.Int64Var(&Flags.SyncInterval, "sync-interval", 5, "timer interval in seconds for reconnection")
	flag.IntVar(&Flags.SyncType, "sync-type", 0, "sync type")
	flag.IntVar(&Flags.SyncId, "sync-id", 0, "sync id")
	flag.BoolVar(&Flags.SyncSecure, "sync-secure", false, "whether sync connection will be secure")

	flag.StringVar(&Flags.HttpHost, "host", "0.0.0.0", "Host to bind HTTP server to")
	flag.StringVar(&Flags.HttpPort, "port", "1080", "Port to bind HTTP server to")
	flag.Int64Var(&Flags.MaxSize, "max-size", 0, "Maximum size of a single upload in bytes")
	flag.StringVar(&Flags.UploadDir, "dir", "./data", "Directory to store uploads in")
	flag.Int64Var(&Flags.StoreSize, "store-size", 0, "Size of space allowed for storage")
	flag.StringVar(&Flags.Basepath, "base-path", "/files/", "Basepath of the HTTP server")
	flag.Int64Var(&Flags.Timeout, "timeout", 30*1000, "Read timeout for connections in milliseconds.  A zero value means that reads will not timeout")
	flag.StringVar(&Flags.S3Bucket, "s3-bucket", "", "Use AWS S3 with this bucket as storage backend (requires the AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_REGION environment variables to be set)")
	flag.StringVar(&Flags.S3Endpoint, "s3-endpoint", "", "Endpoint to use S3 compatible implementations like minio (requires s3-bucket to be pass)")
	flag.StringVar(&Flags.GCSBucket, "gcs-bucket", "", "Use Google Cloud Storage with this bucket as storage backend (requires the GCS_SERVICE_ACCOUNT_FILE environment variable to be set)")
	flag.StringVar(&Flags.FileHooksDir, "hooks-dir", "", "Directory to search for available hooks scripts")
	flag.StringVar(&Flags.HttpHooksEndpoint, "hooks-http", "", "An HTTP endpoint to which hook events will be sent to")
	flag.IntVar(&Flags.HttpHooksRetry, "hooks-http-retry", 3, "Number of times to retry on a 500 or network timeout")
	flag.IntVar(&Flags.HttpHooksBackoff, "hooks-http-backoff", 1, "Number of seconds to wait before retrying each retry")
	flag.BoolVar(&Flags.ShowVersion, "version", false, "Print tusd version information")
	flag.BoolVar(&Flags.ExposeMetrics, "expose-metrics", true, "Expose metrics about tusd usage")
	flag.StringVar(&Flags.MetricsPath, "metrics-path", "/metrics", "Path under which the metrics endpoint will be accessible")
	flag.BoolVar(&Flags.BehindProxy, "behind-proxy", false, "Respect X-Forwarded-* and similar headers which may be set by proxies")

	flag.Parse()
	
	if Flags.SyncDataDir != "" {
		if Flags.SyncType == 0 {
			stderr.Fatalf("The option: -sync-type is required.")
		}

		if Flags.SyncId == 0 {
			stderr.Fatalf("The option: -sync-id is required.")
		}
		
		Flags.SyncEnabled = true
	}
	
	if Flags.FileHooksDir != "" {
		Flags.FileHooksDir, _ = filepath.Abs(Flags.FileHooksDir)
		Flags.FileHooksInstalled = true

		stdout.Printf("Using '%s' for hooks", Flags.FileHooksDir)
	}

	if Flags.HttpHooksEndpoint != "" {
		Flags.HttpHooksInstalled = true

		stdout.Printf("Using '%s' as the endpoint for hooks", Flags.HttpHooksEndpoint)
	}

	if Flags.UploadDir == "" && Flags.S3Bucket == "" {
		stderr.Fatalf("Either an upload directory (using -dir) or an AWS S3 Bucket " +
			"(using -s3-bucket) must be specified to start tusd but " +
			"neither flag was provided. Please consult `tusd -help` for " +
			"more information on these options.")
	}
}
