package webview

import (
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/Eurekaimer/TOUKO/internal/image"
)

// estimateWebP reports the stored size an upload would have with WebP
// conversion enabled. It runs the same ingest code the upload uses, so the
// number includes the rule that keeps the original when WebP would be larger,
// but it stages nothing and keeps no payload.
func (s *Server) estimateWebP(w http.ResponseWriter, r *http.Request) (any, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, usage("multipart/form-data is required: %v", err)
	}
	dir, err := os.MkdirTemp(s.service.Dir, "estimate-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	quality := image.DefaultWebPQuality
	name, path := "", ""
	for {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		part, e := reader.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, bodyError(e)
		}
		if part.FileName() != "" {
			if part.FormName() != "image" {
				return nil, usage("file field must be named image")
			}
			if name != "" {
				return nil, usage("exactly one image may be estimated")
			}
			file, e := os.CreateTemp(dir, "input-")
			if e != nil {
				return nil, e
			}
			n, copyErr := io.Copy(file, io.LimitReader(contextReader{r.Context(), part}, maxImageBytes+1))
			closeErr := file.Close()
			if copyErr != nil {
				return nil, bodyError(copyErr)
			}
			if closeErr != nil {
				return nil, closeErr
			}
			if n > maxImageBytes {
				return nil, tooLarge("image is too large to estimate")
			}
			if n == 0 {
				return nil, usage("image is empty")
			}
			name, path = part.FileName(), file.Name()
			if e = part.Close(); e != nil {
				return nil, bodyError(e)
			}
			continue
		}
		switch part.FormName() {
		case "quality":
			value, e := io.ReadAll(io.LimitReader(contextReader{r.Context(), part}, 16))
			if e != nil {
				return nil, bodyError(e)
			}
			parsed, e := strconv.Atoi(strings.TrimSpace(string(value)))
			if e != nil || parsed < 1 || parsed > 100 {
				return nil, usage("quality must be between 1 and 100")
			}
			quality = parsed
		default:
			return nil, usage("unknown multipart field %q", part.FormName())
		}
		if e = part.Close(); e != nil {
			return nil, bodyError(e)
		}
	}
	if name == "" {
		return nil, usage("select one image")
	}
	source, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	estimate, err := image.EstimateConversion(r.Context(), source, dir, quality)
	closeErr := source.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, usage("invalid image: %v", err)
	}
	return map[string]any{
		"name":            name,
		"original_bytes":  estimate.OriginalSize,
		"estimated_bytes": estimate.EstimatedSize,
		"saved_bytes":     estimate.Saved(),
		"ext":             estimate.Ext,
		"mime":            estimate.MIME,
		"width":           estimate.Width,
		"height":          estimate.Height,
	}, nil
}
