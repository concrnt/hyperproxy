package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/attribute"
)

const (
	CachePath    = "/tmp/hyperproxy"
	MaxCacheSize = 1024 * 1024 * 1024 // 1GB
)

var reCleanedURL = regexp.MustCompile(`^(https?):/+([^/])`)

func CleanDiskCache() int64 {

	err := os.MkdirAll(CachePath, 0755)
	if err != nil {
		err := errors.Wrap(err, "Failed to create cache directory")
		fmt.Println(err)
	}

	entries, err := os.ReadDir(CachePath)
	if err != nil {
		fmt.Println(err)
		return -1
	}

	files := make([]os.FileInfo, 0)

	totalSize := int64(0)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			fmt.Println(err)
			continue
		}

		if info.IsDir() {
			continue
		}

		files = append(files, info)
		totalSize += info.Size()
	}

	if totalSize < MaxCacheSize {
		return totalSize
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].ModTime().Before(files[j].ModTime())
	})

	threadhold := 0.8 * MaxCacheSize

	for _, file := range files {
		if totalSize < int64(threadhold) {
			break
		}
		filePath := filepath.Join(CachePath, file.Name())
		err := os.Remove(filePath)
		if err != nil {
			fmt.Println(err)
			continue
		}
		fmt.Println("Evicted: ", filePath)
		totalSize -= file.Size()
	}

	return totalSize
}

func ImageHandler(c echo.Context) error {
	ctx, span := tracer.Start(c.Request().Context(), "ImageHandler")
	defer span.End()

	// setup cors
	c.Response().Header().Set("Access-Control-Allow-Origin", "*")
	c.Response().Header().Set("Access-Control-Allow-Methods", "GET")

	subpath := strings.TrimPrefix(c.Request().RequestURI, "/image/")

	splitter := strings.Index(subpath, "/")
	if splitter == -1 {
		err := errors.New("Failed to split the path")
		span.RecordError(err)
		return c.String(400, err.Error())
	}
	operator := subpath[:splitter]
	span.SetAttributes(attribute.String("operator", operator))

	split := strings.Split(operator, "x")
	if len(split) != 2 {
		err := errors.New("Failed to split the operator")
		span.RecordError(err)
		return c.String(400, err.Error())
	}

	widthStr := split[0]
	heightStr := split[1]

	width := 0
	if widthStr != "" {
		var err error
		width, err = strconv.Atoi(widthStr)
		if err != nil {
			err := errors.Wrap(err, "Failed to parse width")
			span.RecordError(err)
			return c.String(400, err.Error())
		}
	}

	height := 0
	if heightStr != "" {
		var err error
		height, err = strconv.Atoi(heightStr)
		if err != nil {
			err := errors.Wrap(err, "Failed to parse height")
			span.RecordError(err)
			return c.String(400, err.Error())
		}
	}

	remoteURL := subpath[splitter+1:]
	remoteURL = reCleanedURL.ReplaceAllString(remoteURL, "$1://$2")
	span.SetAttributes(attribute.String("remoteURL", remoteURL))

	fmt.Println("Request:", remoteURL, width, height)

	originalCacheKeyBytes := sha256.Sum256([]byte(remoteURL))
	originalCacheKey := hex.EncodeToString(originalCacheKeyBytes[:])
	originalCachePath := filepath.Join(CachePath, originalCacheKey)

	requestCacheKeyBytes := sha256.Sum256([]byte(c.Request().RequestURI))
	requestCacheKey := hex.EncodeToString(requestCacheKeyBytes[:])
	requestCachePath := filepath.Join(CachePath, requestCacheKey)

	// check cache
	if _, err := os.Stat(requestCachePath + ".data"); err == nil {
		fmt.Println("  Cache hit")
		c.Response().Header().Set("Content-Type", "image/webp")
		c.Response().Header().Set("Cache-Control", "public, max-age=86400, s-maxage=86400, immutable")
		return c.File(requestCachePath + ".data")
	}

	// check if the original image is already cached
	data_cached := false
	header_cached := false

	if _, err := os.Stat(originalCachePath + ".data"); err == nil {
		data_cached = true
	}

	header, err := os.Open(originalCachePath + ".header")
	if err == nil {
		header_cached = true
	}

	resp := &http.Response{}

	if !data_cached || !header_cached {
		fmt.Println("  Fetch Original Image")

		parsedUrl, err := url.Parse(remoteURL)
		if err != nil {
			err := errors.Wrap(err, "Failed to parse URL")
			span.RecordError(err)
			return c.String(400, err.Error())
		}

		if parsedUrl.Scheme != "http" && parsedUrl.Scheme != "https" {
			err := errors.New("Invalid URL scheme")
			span.RecordError(err)
			return c.String(400, err.Error())
		}

		targetHost := parsedUrl.Host
		splitHost, _, err := net.SplitHostPort(parsedUrl.Host)
		if err == nil {
			targetHost = splitHost
		}

		targetIPs, err := net.LookupIP(targetHost)
		if err != nil {
			err := errors.Wrap(err, "Failed to lookup IP")
			span.SetAttributes(attribute.String("host", parsedUrl.Host))
			span.RecordError(err)
			return c.String(400, err.Error())
		}

		whiteListMap := make(map[string]bool)
		var whiteListCIDRs []*net.IPNet
		for _, entry := range IpsWhiteList {
			if _, ipNet, err := net.ParseCIDR(entry); err == nil {
				whiteListCIDRs = append(whiteListCIDRs, ipNet)
			} else {
				whiteListMap[entry] = true
			}
		}

		for _, targetIP := range targetIPs {

			if whiteListMap[targetIP.String()] {
				continue
			}
			inWhiteList := false
			for _, cidr := range whiteListCIDRs {
				if cidr.Contains(targetIP) {
					inWhiteList = true
					break
				}
			}
			if inWhiteList {
				continue
			}

			for _, denyIP := range denyIps {
				_, ipnet, err := net.ParseCIDR(denyIP)
				if err != nil {
					fmt.Println("Error parsing CIDR: ", err)
					span.RecordError(err)
					continue
				}

				if ipnet.Contains(targetIP) {
					err := errors.New("IP is in deny list")
					span.RecordError(err)
					return c.String(403, err.Error())
				}
			}
		}

		_, fetchSpan := tracer.Start(ctx, "FetchImage")

		req, err := http.NewRequest("GET", remoteURL, nil)
		if err != nil {
			err := errors.Wrap(err, "Failed to create request")
			fetchSpan.RecordError(err)
			return c.String(500, err.Error())
		}
		req.Header.Set("User-Agent", useragent)
		resp, err = client.Do(req)
		if err != nil {
			err := errors.Wrap(err, "Failed to fetch image")
			fetchSpan.RecordError(err)
			return c.String(500, err.Error())
		}
		defer resp.Body.Close()

		contentType := resp.Header.Get("Content-Type")

		if resp.StatusCode != 200 {
			err := errors.New("fetch image response code is not 200")
			fetchSpan.SetAttributes(attribute.Int("statusCode", resp.StatusCode))
			fetchSpan.RecordError(err)
			return c.String(resp.StatusCode, err.Error())
		}

		// check if the image is valid
		if !strings.HasPrefix(contentType, "image/") {
			err := errors.New("Invalid image")
			fetchSpan.RecordError(err)
			return c.String(400, err.Error())
		}

		fetchSpan.End()

		// save the image to cache
		err = os.MkdirAll(CachePath, 0755)
		if err != nil {
			err := errors.Wrap(err, "Failed to create cache directory")
			span.RecordError(err)
			return c.String(500, err.Error())
		}

		dataCachePath := originalCachePath + ".data"
		cache, err := os.Create(dataCachePath)
		if err != nil {
			err := errors.Wrap(err, "Failed to create cache file")
			span.RecordError(err)
			return c.String(500, err.Error())
		}
		defer cache.Close()
		io.Copy(cache, resp.Body)

		headerCachePath := originalCachePath + ".header"
		cache, err = os.Create(headerCachePath)
		if err != nil {
			err := errors.Wrap(err, "Failed to create cache file")
			span.RecordError(err)
			return c.String(500, err.Error())
		}
		defer cache.Close()
		resp.Write(cache)
	} else {
		fmt.Println("  Original Image Cache found")
		var err error
		resp, err = http.ReadResponse(bufio.NewReader(header), nil)
		if err != nil {
			err := errors.Wrap(err, "Failed to read response")
			span.RecordError(err)
			return c.String(500, err.Error())
		}
	}

	prefix := ""
	if strings.HasSuffix(remoteURL, ".apng") {
		prefix = "apng:"
	}

	if (width == 0 && height == 0) || (resp.Header.Get("Content-Type") == "image/svg+xml") {
		fmt.Println("  Returning original image")
		c.Response().Header().Set("Cache-Control", "public, max-age=86400, s-maxage=86400, immutable")
		c.Response().Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		return c.File(originalCachePath + ".data")
	}

	ok := resize(prefix+originalCachePath+".data", requestCachePath+".data", width, height)
	if ok != 0 {
		fmt.Println("  [error] Resize Fail Returning original image")
		err := errors.New("Failed to resize image")
		span.RecordError(err)
		c.Response().Header().Set("Cache-Control", "public, max-age=86400, s-maxage=86400, immutable")
		c.Response().Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		return c.File(originalCachePath + ".data")
	}

	fmt.Println("  Returning resized image")
	c.Response().Header().Set("Content-Type", "image/webp")
	c.Response().Header().Set("Cache-Control", "public, max-age=86400, s-maxage=86400, immutable")
	return c.File(requestCachePath + ".data")
}
