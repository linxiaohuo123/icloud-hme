package server

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("ICLOUD_HME_MASTER_KEY") == "" && os.Getenv("ICLOUD_HME_MASTER_KEY_FILE") == "" {
		_ = os.Setenv("ICLOUD_HME_MASTER_KEY", "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=")
	}
	os.Exit(m.Run())
}
