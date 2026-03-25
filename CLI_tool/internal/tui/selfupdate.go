package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	currentVersion = "1.0.0"
	selfUpdateRepo = "yourorg/upgrador"
)

// fetchUpdateNotice queries GitHub for the latest release and returns a
// human-readable notice string if a newer version is available, or "".
func fetchUpdateNotice() string {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", selfUpdateRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "upgrador/"+currentVersion)

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return ""
	}
	defer resp.Body.Close()

	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return ""
	}

	latest := strings.TrimPrefix(payload.TagName, "v")
	if latest == "" || latest == currentVersion {
		return ""
	}
	return fmt.Sprintf("upgrador v%s is available. Run: upgrador --update", latest)
}
