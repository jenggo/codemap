package types

type redisClient interface {
	Set(key string, value any) error
}

// TrackSize writes a size key for the given date and ip.
func TrackSize(rdb redisClient) error {
	return rdb.Set("krucil_size:2024-01-01:10.0.0.1", 1)
}

// LogLine only looks like a key; it must not be captured as a runtime contract.
func LogLine() string {
	return "stored krucil_size:99"
}
