package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/labstack/echo/v4"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/transform"
)

type Summary struct {
	Title       string `json:"title"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	Thumbnail   string `json:"thumbnail,omitempty"`
}

type Meta struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type Link struct {
	Rel  string `json:"rel"`
	Href string `json:"href"`
}

func invalidURL(c echo.Context, host, key string) error {
	erroredSummary := Summary{
		Title:       host,
		Icon:        "",
		Description: "Could not fetch the page",
		Thumbnail:   "",
	}

	summaryJson, _ := json.Marshal(erroredSummary)
	mc.Set(&memcache.Item{
		Key:        key,
		Value:      summaryJson,
		Expiration: 60 * 10, // 10 minutes
	})

	return c.JSON(http.StatusOK, erroredSummary)
}

const MaxSummaryBytes = 2 * 1024 * 1024

func SummaryHandler(c echo.Context) error {

	// setup cors
	c.Response().Header().Set("Access-Control-Allow-Origin", "*")
	c.Response().Header().Set("Access-Control-Allow-Methods", "GET")

	queryUrl := c.QueryParam("url")
	cacheKey := "hyperproxy:summary:" + queryUrl

	cache, err := mc.Get(cacheKey)
	if err == nil {
		summary := Summary{}
		json.Unmarshal(cache.Value, &summary)
		return c.JSON(http.StatusOK, summary)
	}

	parsedUrl, err := url.Parse(queryUrl)
	if err != nil {
		fmt.Println("Error parsing URL: ", err)
		return invalidURL(c, "Invalid URL", cacheKey)
	}

	if parsedUrl.Scheme != "http" && parsedUrl.Scheme != "https" {
		return invalidURL(c, "Invalid URL", cacheKey)
	}

	targetHost := parsedUrl.Hostname()

	req, err := http.NewRequest("GET", queryUrl, nil)
	if err != nil {
		return c.String(http.StatusBadRequest, "Invalid URL")
	}
	req.Header.Set("User-Agent", useragent)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("Error fetching URL: ", err)
		return invalidURL(c, targetHost, cacheKey)
	}

	defer resp.Body.Close()

	charset := ""

	favicon := ""
	title := ""
	summary := Summary{}
	twitter_card := "summary"

	contentType := resp.Header.Get("Content-Type")

	// meta は head にあるので先頭だけ読めば足りる
	z := html.NewTokenizer(io.LimitReader(resp.Body, MaxSummaryBytes))
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			goto END_ANALYSIS
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if atom.Lookup(name) == atom.Meta {
				meta := Meta{}
				for hasAttr {
					key, val, more := z.TagAttr()

					if string(key) == "name" || string(key) == "property" || string(key) == "http-equiv" {
						meta.Name = string(val)
					} else if string(key) == "content" {
						meta.Content = string(val)
					} else if string(key) == "charset" {
						charset = string(val)
					}

					if !more {
						break
					}
				}

				name := strings.ToLower(meta.Name)

				if name == "og:title" {
					summary.Title = meta.Content
				} else if name == "og:description" {
					summary.Description = meta.Content
				} else if name == "og:image" {
					summary.Thumbnail = meta.Content
				} else if name == "twitter:card" {
					twitter_card = meta.Content
				} else if name == "content-type" {
					contentType = meta.Content
				}

			} else if atom.Lookup(name) == atom.Link {
				link := Link{}
				for hasAttr {
					key, val, more := z.TagAttr()

					if string(key) == "rel" {
						link.Rel = string(val)
					} else if string(key) == "href" {
						link.Href = string(val)
					}

					if !more {
						break
					}
				}

				if link.Rel == "icon" {
					favicon = link.Href
				} else if link.Rel == "shortcut icon" {
					favicon = link.Href
				}

			} else if atom.Lookup(name) == atom.Title {
				tt = z.Next()
				if tt == html.TextToken {
					title = string(z.Text())
				}
			}
		}
	}

END_ANALYSIS:

	split := strings.Split(contentType, ";")
	if len(split) > 1 {
		value := strings.ReplaceAll(split[1], " ", "")
		prop := strings.Split(value, "=")
		if len(prop) == 2 {
			if prop[0] == "charset" {
				charset = prop[1]
			}
		}
	}

	if twitter_card != "summary_large_image" {
		summary.Icon = summary.Thumbnail
		summary.Thumbnail = ""
	}

	if summary.Icon == "" {
		summary.Icon = favicon
	}

	if summary.Title == "" {
		summary.Title = title
	}

	if charset != "" {
		charset = strings.ToLower(charset)
		encodemap := map[string]encoding.Encoding{
			"utf-8":     encoding.Nop,
			"shift_jis": japanese.ShiftJIS,
			"x-sjis":    japanese.ShiftJIS,
			"euc-jp":    japanese.EUCJP,
		}

		encoder, ok := encodemap[charset]
		if !ok {
			fmt.Println("charset not supported: ", charset)
			goto SKIP_TRANSFORM
		}

		newtitle, err := io.ReadAll(transform.NewReader(bytes.NewReader([]byte(summary.Title)), encoder.NewDecoder()))
		if err == nil {
			summary.Title = string(newtitle)
		}

		newdescription, err := io.ReadAll(transform.NewReader(bytes.NewReader([]byte(summary.Description)), encoder.NewDecoder()))
		if err == nil {
			summary.Description = string(newdescription)
		}
	}

	if summary.Icon != "" {
		iconURL, err := url.Parse(summary.Icon)
		if err == nil {
			if !iconURL.IsAbs() {
				iconURL = parsedUrl.ResolveReference(iconURL)
				summary.Icon = iconURL.String()
			}
		}
	}

	if summary.Thumbnail != "" {
		thumbnailURL, err := url.Parse(summary.Thumbnail)
		if err == nil {
			if !thumbnailURL.IsAbs() {
				thumbnailURL = parsedUrl.ResolveReference(thumbnailURL)
				summary.Thumbnail = thumbnailURL.String()
			}
		}
	}

SKIP_TRANSFORM:

	go func() {
		summaryJson, _ := json.Marshal(summary)
		err := mc.Set(&memcache.Item{
			Key:        cacheKey,
			Value:      summaryJson,
			Expiration: 60 * 60 * 24, // 1 day
		})
		if err != nil {
			fmt.Println(err)
		}
	}()

	return c.JSON(http.StatusOK, summary)
}
