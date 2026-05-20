package main

import (
	"archive/zip"
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/davidbyttow/govips/v2/vips"
)

//go:embed openapi.yml
var openapiSpec []byte

const swaggerUIHTML = `<!DOCTYPE html>
<html>
<head>
  <title>Image Convert API Docs</title>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>
  SwaggerUIBundle({
    url: "/openapi.yml",
    dom_id: "#swagger-ui",
    presets: [SwaggerUIBundle.presets.apis, SwaggerUIBundle.SwaggerUIStandalonePreset],
    layout: "BaseLayout"
  })
</script>
</body>
</html>`

const port = 12901

var (
	apiRateCount int64

	// ConcurrencyLevel=1 のとき、libvips は 1 スレッドで動作し
	// Go のゴルーチンでリクエストを並列処理する。
	// NumCPU*2 の上限でリソース枯渇を防ぐ。
	convertSem = make(chan struct{}, runtime.NumCPU()*2)

	// バッファプールで GC 負荷を軽減
	bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}
)

func getMimeType(format string) string {
	switch strings.ToLower(format) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "tif", "tiff":
		return "image/tiff"
	case "svg":
		return "image/svg+xml"
	default:
		return "image/" + strings.ToLower(format)
	}
}

func buildContentDisposition(fileName string) string {
	replacer := strings.NewReplacer(`"`, "_", `\`, "_")
	asciiFileName := replacer.Replace(fileName)
	utf8FileName := url.PathEscape(fileName)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, asciiFileName, utf8FileName)
}

func buildUniqueFileName(fileName string, usedNames map[string]struct{}) string {
	ext := filepath.Ext(fileName)
	base := strings.TrimSuffix(fileName, ext)
	candidate := fileName
	counter := 1
	for {
		if _, exists := usedNames[candidate]; !exists {
			break
		}
		candidate = fmt.Sprintf("%s (%d)%s", base, counter, ext)
		counter++
	}
	usedNames[candidate] = struct{}{}
	return candidate
}

func parseImageType(format string) (vips.ImageType, error) {
	format = strings.ToLower(strings.TrimPrefix(format, "."))
	switch format {
	case "webp":
		return vips.ImageTypeWEBP, nil
	case "jpg", "jpeg":
		return vips.ImageTypeJPEG, nil
	case "png":
		return vips.ImageTypePNG, nil
	case "gif":
		return vips.ImageTypeGIF, nil
	case "tif", "tiff":
		return vips.ImageTypeTIFF, nil
	case "avif":
		return vips.ImageTypeAVIF, nil
	case "heif", "heic":
		return vips.ImageTypeHEIF, nil
	default:
		return vips.ImageTypeUnknown, fmt.Errorf("unsupported format: %s", format)
	}
}

func convertImage(ctx context.Context, data []byte, imgType vips.ImageType) ([]byte, error) {
	// セマフォ取得：コンテキストキャンセル時は即中断
	select {
	case convertSem <- struct{}{}:
		defer func() { <-convertSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	img, err := vips.NewImageFromBuffer(data)
	if err != nil {
		return nil, fmt.Errorf("load image: %w", err)
	}
	defer img.Close()

	ep := vips.NewDefaultExportParams()
	ep.Format = imgType

	buf, _, err := img.Export(ep)
	if err != nil {
		return nil, fmt.Errorf("export image: %w", err)
	}
	return buf, nil
}

func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

func handleConvert(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	atomic.AddInt64(&apiRateCount, 1)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	fileHeaders := r.MultipartForm.File["images"]
	if len(fileHeaders) == 0 {
		http.Error(w, "No images uploaded.", http.StatusBadRequest)
		return
	}

	toFormat := strings.ToLower(r.URL.Query().Get("toFormat"))
	if toFormat == "" {
		toFormat = "webp"
	}

	imgType, err := parseImageType(toFormat)
	if err != nil {
		http.Error(w, "Unsupported format: "+toFormat, http.StatusBadRequest)
		return
	}

	type result struct {
		fileName string
		data     []byte
		err      error
	}

	results := make([]result, len(fileHeaders))
	var wg sync.WaitGroup

	for i, fh := range fileHeaders {
		wg.Add(1)
		go func(i int, fh *multipart.FileHeader) {
			defer wg.Done()
			f, err := fh.Open()
			if err != nil {
				results[i] = result{err: fmt.Errorf("open: %w", err)}
				return
			}
			defer f.Close()

			buf := bufPool.Get().(*bytes.Buffer)
			buf.Reset()
			defer bufPool.Put(buf)

			if _, err := io.Copy(buf, f); err != nil {
				results[i] = result{err: fmt.Errorf("read: %w", err)}
				return
			}

			converted, err := convertImage(r.Context(), buf.Bytes(), imgType)
			if err != nil {
				results[i] = result{err: err}
				return
			}

			results[i] = result{
				fileName: fh.Filename,
				data:     converted,
			}
		}(i, fh)
	}

	wg.Wait()

	for _, res := range results {
		if res.err != nil {
			log.Printf("conversion error: %v", res.err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	}

	if len(results) == 1 {
		w.Header().Set("Content-Type", getMimeType(toFormat))
		w.Header().Set("Content-Disposition", buildContentDisposition(results[0].fileName))
		w.WriteHeader(http.StatusOK)
		w.Write(results[0].data)
		return
	}

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	usedNames := make(map[string]struct{})

	for _, res := range results {
		uniqueName := buildUniqueFileName(res.fileName, usedNames)
		fw, err := zw.Create(uniqueName)
		if err != nil {
			log.Printf("zip create error: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		if _, err := fw.Write(res.data); err != nil {
			log.Printf("zip write error: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	}

	if err := zw.Close(); err != nil {
		log.Printf("zip close error: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="converted_images.zip"`)
	w.WriteHeader(http.StatusOK)
	w.Write(zipBuf.Bytes())
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "OK")
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	count := atomic.LoadInt64(&apiRateCount)
	fmt.Fprintf(w, "ApiRateCount %d", count)
}

func handleDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, swaggerUIHTML)
}

func handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Write(openapiSpec)
}

func main() {
	vips.Startup(&vips.Config{
		ConcurrencyLevel: 1,                 // libvips は 1 スレッド固定、並列性は Go 側で管理
		MaxCacheFiles:    0,                 // ファイルキャッシュ無効
		MaxCacheMem:      100 * 1024 * 1024, // 100MB メモリキャッシュ
		MaxCacheSize:     500,
		ReportLeaks:      false,
	})
	defer vips.Shutdown()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/convert", handleConvert)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/metrics", handleMetrics)
	mux.HandleFunc("/docs", handleDocs)
	mux.HandleFunc("/openapi.yml", handleOpenAPISpec)

	srv := &http.Server{
		Addr:           fmt.Sprintf(":%d", port),
		Handler:        mux,
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   120 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1MB
	}

	// グレースフルシャットダウン
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("Server is running on port %d", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	<-quit
	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}
	log.Println("Server stopped")
}
