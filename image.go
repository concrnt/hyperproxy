package main

import (
	"bufio"
	"bytes"
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

	_ "github.com/jdeng/goheif"
	"github.com/kettek/apng"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
	"image"
	"image/gif"
	_ "image/jpeg"

	"github.com/chai2010/webp"
	"github.com/disintegration/imaging"
	"github.com/labstack/echo/v4"
	"github.com/pkg/errors"
	"github.com/rwcarlsen/goexif/exif"
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

	requestCacheKeyBytes := sha256.Sum256([]byte(remoteURL))
	requestCacheKey := hex.EncodeToString(requestCacheKeyBytes[:])
	requestCachePath := filepath.Join(CachePath, requestCacheKey)

	// Check if the original image is already cached
	if _, err := os.Stat(requestCachePath); err == nil {
		fmt.Println("Cache hit: ", remoteURL)
	} else {

		fmt.Println("Cache miss: ", remoteURL)

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

		for _, denyIP := range denyIps {
			_, ipnet, err := net.ParseCIDR(denyIP)
			if err != nil {
				fmt.Println("Error parsing CIDR: ", err)
				span.RecordError(err)
				continue
			}

			for _, targetIP := range targetIPs {
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
			span.RecordError(err)
			return c.String(500, err.Error())
		}
		req.Header.Set("User-Agent", useragent)
		resp, err := client.Do(req)
		if err != nil {
			err := errors.Wrap(err, "Failed to fetch image")
			span.RecordError(err)
			return c.String(500, err.Error())
		}
		defer resp.Body.Close()
		fetchSpan.End()

		if resp.StatusCode != 200 {
			err := errors.New("fetch image response code is not 200")
			span.SetAttributes(attribute.Int("statusCode", resp.StatusCode))

			body, _ := io.ReadAll(resp.Body)
			span.SetAttributes(attribute.String("body", string(body)))

			span.RecordError(err)
			return c.String(resp.StatusCode, err.Error())
		}

		// check if the image is valid
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/") {
			err := errors.New("Invalid image")
			span.RecordError(err)
			return c.String(400, err.Error())
		}

		// save the image to cache
		err = os.MkdirAll(CachePath, 0755)
		if err != nil {
			err := errors.Wrap(err, "Failed to create cache directory")
			span.RecordError(err)
			return c.String(500, err.Error())
		}

		cacheFile, err := os.Create(requestCachePath)
		if err != nil {
			err := errors.Wrap(err, "Failed to create cache file")
			span.RecordError(err)
			return c.String(500, err.Error())
		}
		defer cacheFile.Close()

		resp.Write(cacheFile)
	}

	cache, err := os.Open(requestCachePath)
	if err != nil {
		err := errors.Wrap(err, "Failed to open original cache")
		span.RecordError(err)
		return c.String(500, err.Error())
	}

	reader := bufio.NewReader(cache)

	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		err := errors.Wrap(err, "Failed to read response")
		span.RecordError(err)
		return c.String(500, err.Error())
	}
	defer resp.Body.Close()

	bodyBytes, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		err := errors.Wrap(err, "Failed to read body")
		span.RecordError(err)
	}

	contentType := resp.Header.Get("Content-Type")

	cacheStat, err := cache.Stat()
	if err != nil {
		err := errors.Wrap(err, "Failed to stat cache")
		span.RecordError(err)
		return c.String(500, err.Error())
	}

	bodyStartOffset := cacheStat.Size() - bodyBytes

	// load image
	_, loadSpan := tracer.Start(ctx, "LoadImage")

	cache.Seek(bodyStartOffset, io.SeekStart)
	img, format, err := image.Decode(cache)

	// check if the image is animated
	isAnimated := false
	if err == nil {
		switch format {
		case "gif":
			cache.Seek(bodyStartOffset, io.SeekStart)

			gifImg, err := gif.DecodeAll(cache)
			if err == nil && len(gifImg.Image) > 1 {
				isAnimated = true
			}
		case "apng":
			cache.Seek(bodyStartOffset, io.SeekStart)

			apngImg, err := apng.DecodeAll(cache)
			if err == nil && len(apngImg.Frames) > 1 {
				isAnimated = true
			}
		}
	}

	if err != nil || isAnimated {
		if err != nil {
			fmt.Printf("Fallback to original image: %s (%s) %s\n", remoteURL, format, err)
		}
		c.Response().Header().Set("Cache-Control", "public, max-age=86400, s-maxage=86400, immutable")
		cache.Seek(bodyStartOffset, io.SeekStart)
		return c.Stream(200, contentType, io.MultiReader(cache, reader))
	}
	loadSpan.End()

	orientation := 1
	if format == "jpeg" {
		cache.Seek(bodyStartOffset, io.SeekStart)
		exifData, err := exif.Decode(cache)
		if err == nil {
			exifOrient, err := exifData.Get(exif.Orientation)
			if err == nil {
				orientation, err = exifOrient.Int(0)
				if err != nil {
					fmt.Println("Error parsing orientation: ", err)
				}
			}
		}
	}

	originalWidth := img.Bounds().Dx()
	originalHeight := img.Bounds().Dy()

	if orientation >= 5 {
		originalWidth, originalHeight = originalHeight, originalWidth
	}

	resizeWidth := width
	resizeHeight := height

	if resizeWidth > originalWidth {
		resizeWidth = originalWidth
	}

	if resizeHeight > originalHeight {
		resizeHeight = originalHeight
	}

	// resize image
	_, resizeSpan := tracer.Start(ctx, "ResizeImage")

	switch orientation {
	case 2:
		img = imaging.FlipH(img)
	case 3:
		img = imaging.Rotate180(img)
	case 4:
		img = imaging.FlipV(img)
	case 5:
		img = imaging.Transpose(img)
	case 6:
		img = imaging.Rotate270(img)
	case 7:
		img = imaging.Transverse(img)
	case 8:
		img = imaging.Rotate90(img)
	}

	if (resizeWidth == 0 || resizeWidth == originalWidth) && (resizeHeight == 0 || resizeHeight == originalHeight) {
		// no need to resize
	} else {
		img = imaging.Resize(img, resizeWidth, resizeHeight, imaging.CatmullRom)
	}

	resizeSpan.End()

	// encode image
	_, encodeSpan := tracer.Start(ctx, "EncodeImage")
	var buff bytes.Buffer
	err = webp.Encode(&buff, img, &webp.Options{Quality: 80})
	if err != nil {
		err := errors.Wrap(err, "Failed to encode image")
		span.RecordError(err)
		return c.String(500, err.Error())
	}
	encodeSpan.End()

	// return the image
	c.Response().Header().Set("Cache-Control", "public, max-age=86400, s-maxage=86400, immutable")
	return c.Stream(200, "image/webp", &buff)
}
