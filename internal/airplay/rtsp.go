package airplay

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/textproto"
	"strconv"
	"strings"

	"howett.net/plist"
)

// request is one RTSP (or, for GET /info, HTTP) request on the control
// connection.
type request struct {
	Method, Path, Proto string
	Header              textproto.MIMEHeader
	Body                []byte
}

const maxBody = 4 << 20

func readRequest(r *bufio.Reader) (*request, error) {
	tp := textproto.NewReader(r)
	line, err := tp.ReadLine()
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("rtsp: malformed request line %q", line)
	}
	hdr, err := tp.ReadMIMEHeader()
	if err != nil && err != io.EOF {
		return nil, err
	}
	req := &request{Method: parts[0], Path: parts[1], Proto: parts[2], Header: hdr}
	if path, _, ok := strings.Cut(req.Path, "?"); ok {
		req.Path = path
	}
	if cl := hdr.Get("Content-Length"); cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil || n < 0 || n > maxBody {
			return nil, fmt.Errorf("rtsp: bad Content-Length %q", cl)
		}
		req.Body = make([]byte, n)
		if _, err := io.ReadFull(r, req.Body); err != nil {
			return nil, err
		}
	}
	return req, nil
}

// plistBody decodes a binary (or XML) plist body into a map.
func (r *request) plistBody() (map[string]any, error) {
	if len(r.Body) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if _, err := plist.Unmarshal(r.Body, &m); err != nil {
		return nil, err
	}
	return m, nil
}

type response struct {
	Status      int
	Header      map[string]string
	ContentType string
	Body        []byte
}

var statusText = map[int]string{200: "OK", 400: "Bad Request", 404: "Not Found", 453: "Not Enough Bandwidth", 470: "Connection Authorization Required", 500: "Internal Server Error", 501: "Not Implemented"}

func (resp *response) write(w io.Writer, req *request) error {
	var b bytes.Buffer
	text := statusText[resp.Status]
	if text == "" {
		text = "Unknown"
	}
	fmt.Fprintf(&b, "RTSP/1.0 %d %s\r\n", resp.Status, text)
	if cseq := req.Header.Get("CSeq"); cseq != "" {
		fmt.Fprintf(&b, "CSeq: %s\r\n", cseq)
	}
	fmt.Fprintf(&b, "Server: AirTunes/%s\r\n", sourceVersion)
	for k, v := range resp.Header {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	if len(resp.Body) > 0 {
		if resp.ContentType != "" {
			fmt.Fprintf(&b, "Content-Type: %s\r\n", resp.ContentType)
		}
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(resp.Body))
	}
	b.WriteString("\r\n")
	b.Write(resp.Body)
	_, err := w.Write(b.Bytes())
	return err
}

func plistResponse(v any) (*response, error) {
	body, err := plist.Marshal(v, plist.BinaryFormat)
	if err != nil {
		return nil, err
	}
	return &response{Status: 200, ContentType: "application/x-apple-binary-plist", Body: body}, nil
}

func plistUnmarshal(b []byte, v any) (int, error) { return plist.Unmarshal(b, v) }

// Plist value helpers: numbers arrive as uint64, int64 or float64.

func pInt(m map[string]any, key string) (int64, bool) {
	switch v := m[key].(type) {
	case uint64:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return int64(v), true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func pUint(m map[string]any, key string) (uint64, bool) {
	switch v := m[key].(type) {
	case uint64:
		return v, true
	case int64:
		return uint64(v), true
	case float64:
		return uint64(v), true
	}
	return 0, false
}

func pFloat(m map[string]any, key string) (float64, bool) {
	switch v := m[key].(type) {
	case float64:
		return v, true
	case uint64:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

func pString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func pBytes(m map[string]any, key string) []byte {
	b, _ := m[key].([]byte)
	return b
}
