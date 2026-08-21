package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	episodesDir   = "content/episodes"
	defaultRSSURL = "https://anchor.fm/s/10af857f4/podcast/rss"
)

var nonSlugChars = regexp.MustCompile(`[^a-z0-9]+`)

type rssRoot struct {
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title string    `xml:"title"`
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	Title          string       `xml:"title"`
	Description    string       `xml:"description"`
	ContentEncoded string       `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
	PubDate        string       `xml:"pubDate"`
	GUID           string       `xml:"guid"`
	Link           string       `xml:"link"`
	Enclosure      rssEnclosure `xml:"enclosure"`
	Duration       string       `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd duration"`
	Episode        string       `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd episode"`
	EpisodeType    string       `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd episodeType"`
	Explicit       string       `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd explicit"`
}

type rssEnclosure struct {
	URL  string `xml:"url,attr"`
	Type string `xml:"type,attr"`
}

func main() {
	rssURL := flag.String("rss-url", envOrDefault("PODCAST_RSS_URL", defaultRSSURL), "Podcast RSS feed URL")
	flag.Parse()

	client := &http.Client{Timeout: 30 * time.Second}
	feed, err := fetchRSS(client, *rssURL)
	if err != nil {
		exitWithErr(err)
	}

	episodes := toEpisodes(feed.Channel.Items)
	sort.Slice(episodes, func(i, j int) bool { return episodes[i].Published.After(episodes[j].Published) })

	if err := writeEpisodes(feed.Channel.Title, episodes); err != nil {
		exitWithErr(err)
	}

	fmt.Printf("synced %d episodes from %q\n", len(episodes), *rssURL)
}

func exitWithErr(err error) {
	fmt.Fprintf(os.Stderr, "episode-sync: %v\n", err)
	os.Exit(1)
}

func envOrDefault(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func fetchRSS(client *http.Client, rawURL string) (*rssRoot, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("parse RSS URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("RSS URL must be absolute: %q", rawURL)
	}

	resp, err := client.Get(parsed.String())
	if err != nil {
		return nil, fmt.Errorf("request RSS feed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("RSS response status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read RSS response: %w", err)
	}

	var root rssRoot
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse RSS XML: %w", err)
	}
	if len(root.Channel.Items) == 0 {
		return nil, errors.New("RSS feed contains no episodes")
	}

	return &root, nil
}

type episodeContent struct {
	ID          string
	Title       string
	Description string
	Published   time.Time
	Duration    string
	EpisodeNo   string
	EpisodeType string
	Explicit    bool
	PageURL     string
	AudioURL    string
}

func toEpisodes(items []rssItem) []episodeContent {
	episodes := make([]episodeContent, 0, len(items))
	for _, item := range items {
		published := parsePubDate(item.PubDate)
		description := strings.TrimSpace(item.Description)
		if description == "" {
			description = strings.TrimSpace(item.ContentEncoded)
		}
		if description == "" {
			description = "No description provided by the RSS feed."
		}

		idSource := strings.TrimSpace(item.GUID)
		if idSource == "" {
			idSource = item.Link + "|" + item.Title + "|" + item.PubDate
		}

		episodes = append(episodes, episodeContent{
			ID:          shortHash(idSource),
			Title:       strings.TrimSpace(item.Title),
			Description: description,
			Published:   published,
			Duration:    strings.TrimSpace(item.Duration),
			EpisodeNo:   strings.TrimSpace(item.Episode),
			EpisodeType: strings.TrimSpace(item.EpisodeType),
			Explicit:    strings.EqualFold(strings.TrimSpace(item.Explicit), "yes"),
			PageURL:     strings.TrimSpace(item.Link),
			AudioURL:    strings.TrimSpace(item.Enclosure.URL),
		})
	}
	return episodes
}

func parsePubDate(value string) time.Time {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Now().UTC()
	}

	formats := []string{
		time.RFC1123Z,
		time.RFC1123,
		time.RFC822Z,
		time.RFC822,
		time.RFC3339,
	}

	for _, format := range formats {
		if parsed, err := time.Parse(format, trimmed); err == nil {
			return parsed.UTC()
		}
	}

	return time.Now().UTC()
}

func shortHash(input string) string {
	sum := sha1.Sum([]byte(input))
	return fmt.Sprintf("%x", sum[:6])
}

func writeEpisodes(showName string, episodes []episodeContent) error {
	if err := os.MkdirAll(episodesDir, 0o755); err != nil {
		return fmt.Errorf("create episodes directory: %w", err)
	}

	oldGeneratedFiles, err := filepath.Glob(filepath.Join(episodesDir, "rss-*.md"))
	if err != nil {
		return fmt.Errorf("list generated episode files: %w", err)
	}
	for _, path := range oldGeneratedFiles {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove old generated file %s: %w", path, err)
		}
	}

	for _, episode := range episodes {
		published := episode.Published
		filename := fmt.Sprintf("rss-%s-%s.md", published.Format("2006-01-02"), sanitizeSlug(episode.ID))
		path := filepath.Join(episodesDir, filename)

		content := renderEpisodeMarkdown(showName, episode)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}

	return nil
}

func renderEpisodeMarkdown(showName string, episode episodeContent) string {
	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.WriteString("title: ")
	buf.WriteString(strconv.Quote(episode.Title))
	buf.WriteString("\n")
	buf.WriteString("date: ")
	buf.WriteString(episode.Published.UTC().Format(time.RFC3339))
	buf.WriteString("\n")
	buf.WriteString("draft: false\n")
	buf.WriteString("source: \"rss\"\n")
	buf.WriteString("sync_id: ")
	buf.WriteString(strconv.Quote(episode.ID))
	buf.WriteString("\n")
	buf.WriteString("show: ")
	buf.WriteString(strconv.Quote(showName))
	buf.WriteString("\n")
	if episode.Duration != "" {
		buf.WriteString("duration: ")
		buf.WriteString(strconv.Quote(episode.Duration))
		buf.WriteString("\n")
	}
	if episode.EpisodeNo != "" {
		buf.WriteString("episode_number: ")
		buf.WriteString(strconv.Quote(episode.EpisodeNo))
		buf.WriteString("\n")
	}
	if episode.EpisodeType != "" {
		buf.WriteString("episode_type: ")
		buf.WriteString(strconv.Quote(episode.EpisodeType))
		buf.WriteString("\n")
	}
	buf.WriteString("explicit: ")
	buf.WriteString(strconv.FormatBool(episode.Explicit))
	buf.WriteString("\n")
	buf.WriteString("---\n\n")
	buf.WriteString(episode.Description)

	sections := make([]string, 0, 2)
	if episode.PageURL != "" {
		sections = append(sections, "Episode page: ["+escapeMarkdownLabel(episode.Title)+"]("+episode.PageURL+")")
	}
	if episode.AudioURL != "" {
		sections = append(sections, "<audio controls preload=\"none\" src=\""+episode.AudioURL+"\">Your browser does not support the audio element.</audio>")
	}
	if len(sections) > 0 {
		buf.WriteString("\n\n")
		buf.WriteString(strings.Join(sections, "\n\n"))
	}

	buf.WriteString("\n")
	return buf.String()
}

func sanitizeSlug(input string) string {
	slug := strings.ToLower(strings.TrimSpace(input))
	slug = nonSlugChars.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return "episode"
	}
	return slug
}

func escapeMarkdownLabel(input string) string {
	label := strings.ReplaceAll(input, "[", "\\[")
	label = strings.ReplaceAll(label, "]", "\\]")
	return label
}
