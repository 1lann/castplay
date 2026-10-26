package mrp

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"

	"golang.org/x/image/draw"
)

// FetchArtwork downloads an image and re-encodes it as a JPEG at most width
// pixels wide, keeping MRP messages small.
func FetchArtwork(ctx context.Context, url string, width int) (*Artwork, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("artwork: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return ScaleArtwork(b, width)
}

// ScaleArtwork re-encodes an image as a JPEG at most width pixels wide.
func ScaleArtwork(b []byte, width int) (*Artwork, error) {
	src, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("artwork: %w", err)
	}
	img := src
	if sb := src.Bounds(); sb.Dx() > width {
		h := sb.Dy() * width / sb.Dx()
		dst := image.NewRGBA(image.Rect(0, 0, width, h))
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, sb, draw.Src, nil)
		img = dst
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return &Artwork{Data: out.Bytes(), MIME: "image/jpeg", Width: img.Bounds().Dx(), Height: img.Bounds().Dy()}, nil
}
