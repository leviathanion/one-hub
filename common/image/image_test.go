package image_test

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	img "one-api/common/image"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type CountingReader struct {
	reader    io.Reader
	BytesRead int
}

func (r *CountingReader) Read(p []byte) (n int, err error) {
	n, err = r.reader.Read(p)
	r.BytesRead += n
	return n, err
}

var (
	cases []struct {
		url    string
		format string
		width  int
		height int
	}
)

func TestMain(m *testing.M) {
	server := newImageFixtureServer()
	defer server.Close()
	cases = []struct {
		url    string
		format string
		width  int
		height int
	}{
		{server.URL + "/fixture.jpg", "jpeg", 32, 21},
		{server.URL + "/fixture.png", "png", 45, 26},
		{server.URL + "/fixture.gif", "gif", 19, 15},
	}
	os.Exit(m.Run())
}

func newImageFixtureServer() *httptest.Server {
	fixtures := map[string]struct {
		contentType string
		data        []byte
	}{
		"/fixture.jpg": {"image/jpeg", encodeTestImage("jpeg", 32, 21)},
		"/fixture.png": {"image/png", encodeTestImage("png", 45, 26)},
		"/fixture.gif": {"image/gif", encodeTestImage("gif", 19, 15)},
		"/not-image":   {"text/plain", []byte("not an image")},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture, ok := fixtures[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", fixture.contentType)
		_, _ = w.Write(fixture.data)
	}))
}

func encodeTestImage(format string, width int, height int) []byte {
	rect := image.Rect(0, 0, width, height)
	palette := []color.Color{color.RGBA{R: 0x20, G: 0x80, B: 0xc0, A: 0xff}}
	img := image.NewPaletted(rect, palette)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetColorIndex(x, y, 0)
		}
	}
	var buf bytes.Buffer
	switch format {
	case "jpeg":
		if err := jpeg.Encode(&buf, img, nil); err != nil {
			panic(err)
		}
	case "png":
		if err := png.Encode(&buf, img); err != nil {
			panic(err)
		}
	case "gif":
		if err := gif.Encode(&buf, img, nil); err != nil {
			panic(err)
		}
	default:
		panic("unknown image test format")
	}
	return buf.Bytes()
}

func TestDecode(t *testing.T) {
	// Bytes read: varies sometimes
	// jpeg: 1063892
	// png: 294462
	// webp: 99529
	// gif: 956153
	// jpeg#01: 32805
	for _, c := range cases {
		t.Run("Decode:"+c.format, func(t *testing.T) {
			resp, err := http.Get(c.url)
			require.NoError(t, err)
			defer resp.Body.Close()
			reader := &CountingReader{reader: resp.Body}
			img, format, err := image.Decode(reader)
			require.NoError(t, err)
			size := img.Bounds().Size()
			assert.Equal(t, c.format, format)
			assert.Equal(t, c.width, size.X)
			assert.Equal(t, c.height, size.Y)
			t.Logf("Bytes read: %d", reader.BytesRead)
		})
	}

	// Bytes read:
	// jpeg: 4096
	// png: 4096
	// webp: 4096
	// gif: 4096
	// jpeg#01: 4096
	for _, c := range cases {
		t.Run("DecodeConfig:"+c.format, func(t *testing.T) {
			resp, err := http.Get(c.url)
			require.NoError(t, err)
			defer resp.Body.Close()
			reader := &CountingReader{reader: resp.Body}
			config, format, err := image.DecodeConfig(reader)
			require.NoError(t, err)
			assert.Equal(t, c.format, format)
			assert.Equal(t, c.width, config.Width)
			assert.Equal(t, c.height, config.Height)
			t.Logf("Bytes read: %d", reader.BytesRead)
		})
	}
}

func TestBase64(t *testing.T) {
	// Bytes read:
	// jpeg: 1063892
	// png: 294462
	// webp: 99072
	// gif: 953856
	// jpeg#01: 32805
	for _, c := range cases {
		t.Run("Decode:"+c.format, func(t *testing.T) {
			resp, err := http.Get(c.url)
			require.NoError(t, err)
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			encoded := base64.StdEncoding.EncodeToString(data)
			body := base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded))
			reader := &CountingReader{reader: body}
			img, format, err := image.Decode(reader)
			require.NoError(t, err)
			size := img.Bounds().Size()
			assert.Equal(t, c.format, format)
			assert.Equal(t, c.width, size.X)
			assert.Equal(t, c.height, size.Y)
			t.Logf("Bytes read: %d", reader.BytesRead)
		})
	}

	// Bytes read:
	// jpeg: 1536
	// png: 768
	// webp: 768
	// gif: 1536
	// jpeg#01: 3840
	for _, c := range cases {
		t.Run("DecodeConfig:"+c.format, func(t *testing.T) {
			resp, err := http.Get(c.url)
			require.NoError(t, err)
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			encoded := base64.StdEncoding.EncodeToString(data)
			body := base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded))
			reader := &CountingReader{reader: body}
			config, format, err := image.DecodeConfig(reader)
			require.NoError(t, err)
			assert.Equal(t, c.format, format)
			assert.Equal(t, c.width, config.Width)
			assert.Equal(t, c.height, config.Height)
			t.Logf("Bytes read: %d", reader.BytesRead)
		})
	}
}

func TestGetImageSize(t *testing.T) {
	for i, c := range cases {
		t.Run("Decode:"+strconv.Itoa(i), func(t *testing.T) {
			width, height, err := img.GetImageSize(c.url)
			assert.NoError(t, err)
			assert.Equal(t, c.width, width)
			assert.Equal(t, c.height, height)
		})
	}
}

func TestGetImageSizeFromBase64(t *testing.T) {
	for i, c := range cases {
		t.Run("Decode:"+strconv.Itoa(i), func(t *testing.T) {
			resp, err := http.Get(c.url)
			require.NoError(t, err)
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			encoded := base64.StdEncoding.EncodeToString(data)
			width, height, err := img.GetImageSizeFromBase64(encoded)
			assert.NoError(t, err)
			assert.Equal(t, c.width, width)
			assert.Equal(t, c.height, height)
		})
	}
}

func TestGetImageFromUrl(t *testing.T) {
	for i, c := range cases {
		t.Run("Decode:"+strconv.Itoa(i), func(t *testing.T) {
			resp, err := http.Get(c.url)
			require.NoError(t, err)
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			encoded := base64.StdEncoding.EncodeToString(data)

			mimeType, base64Data, err := img.GetImageFromUrl(c.url)
			require.NoError(t, err)
			assert.Equal(t, encoded, base64Data)
			assert.Equal(t, "image/"+c.format, mimeType)

			encodedBase64 := "data:image/" + c.format + ";base64," + encoded
			mimeType, base64Data, err = img.GetImageFromUrl(encodedBase64)
			assert.NoError(t, err)
			assert.Equal(t, encoded, base64Data)
			assert.Equal(t, "image/"+c.format, mimeType)
		})
	}

	_, _, err := img.GetImageFromUrl("ftp://example.invalid/image.png")
	assert.Error(t, err)
	encodedBase64 := "data:image/text;base64,"
	_, _, err = img.GetImageFromUrl(encodedBase64)
	assert.Error(t, err)
}

func TestParseBase64File(t *testing.T) {
	mimeType, data, err := img.ParseBase64File("data:audio/mpeg;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=")
	assert.NoError(t, err)
	assert.Equal(t, "audio/mpeg", mimeType)
	assert.Equal(t, "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=", data)
}
