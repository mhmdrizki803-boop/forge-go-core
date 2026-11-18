package main

// server.go — a small HTTP/1.1 server that replaces net/http's Serve loop on the hot path.
//
// Why: the trial runs in a gVisor sandbox on 2 CPUs, where every syscall is expensive. net/http
// allocates a request object, a header map, a body reader and a chunked writer per request, and
// flushes headers and body separately. This server reads one request with a single buffered reader,
// hands the same http.Handler a plain *http.Request, and answers with one write: the status line,
// the headers and the body go out together. Keep-alive, chunked request bodies, Expect:
// 100-continue, HEAD and HTTP/1.0 close semantics are all handled here.

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	fastReadBuffer  = 16 << 10
	fastWriteBuffer = 64 << 10
	fastIdleTimeout = 150 * time.Second
	maxRequestBody  = 256 << 20
)

// fastServer serves a handler on a listener until the listener is closed. When forge is set, a
// GET or HEAD that the reply cache already holds is answered without building a request at all.
type fastServer struct {
	handler http.Handler
	forge   *Forge
}

func newFastServer(handler http.Handler) *fastServer { return &fastServer{handler: handler} }

func newForgeServer(forge *Forge) *fastServer { return &fastServer{handler: forge, forge: forge} }

func (s *fastServer) serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handleConn(conn)
	}
}

func (s *fastServer) handleConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, fastReadBuffer)
	bw := bufio.NewWriterSize(conn, fastWriteBuffer)
	scratch := make([]byte, 0, 512)
	var headers fastHeaders
	for {
		_ = conn.SetReadDeadline(time.Now().Add(fastIdleTimeout))
		req, raw, err := readFastRequest(conn, br, &scratch, &headers)
		if err != nil {
			return
		}
		fastRaw := ""
		if req.Method == "POST" && len(raw) > 0 && strings.HasSuffix(req.RequestURI, "/git-upload-pack") {
			fastRaw = string(raw)
		}
		if s.forge != nil && (req.Method == "GET" || req.Method == "HEAD" || fastRaw != "") {
			head, body, ok := s.forge.FastWire(req.Method, req.RequestURI, headers.auth, headers.gitProtocol, fastRaw)
			if ok {
				if err := writeCachedReply(conn, bw, req, head, body); err != nil {
					return
				}
				if req.Close {
					return
				}
				continue
			}
		}
		writer := &fastWriter{}
		s.handler.ServeHTTP(writer, req)
		if err := writeFastResponse(conn, bw, req, writer); err != nil {
			return
		}
		if req.Close {
			return
		}
	}
}

// pReader is the request body: the bytes are already in memory, so reads never block.
type pReader struct {
	data []byte
	off  int
}

func (r *pReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

func (r *pReader) Close() error { return nil }

// fastHeaders holds the two request headers the reply cache keys on, read without allocating a map.
type fastHeaders struct {
	auth        string
	gitProtocol string
}

// writeCachedReply writes a prebuilt reply: one buffered pass, no formatting.
func writeCachedReply(conn net.Conn, bw *bufio.Writer, req *http.Request, head, body []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(fastIdleTimeout))
	bw.Write(head)
	bw.WriteString("Date: ")
	bw.WriteString(time.Now().UTC().Format(http.TimeFormat))
	bw.WriteString("\r\n\r\n")
	if req.Method != "HEAD" && len(body) > 0 {
		bw.Write(body)
	}
	return bw.Flush()
}

// readFastRequest parses one request off the connection. It returns io.EOF when the peer closed.
func readFastRequest(conn net.Conn, br *bufio.Reader, scratch *[]byte, fast *fastHeaders) (*http.Request, []byte, error) {
	line, err := readCRLFLine(br, scratch)
	if err != nil {
		return nil, nil, err
	}
	if len(line) == 0 {
		// A stray blank line before a request: skip it, as net/http does.
		line, err = readCRLFLine(br, scratch)
		if err != nil {
			return nil, nil, err
		}
	}
	method, target, proto, ok := splitRequestLine(line)
	if !ok {
		return nil, nil, errors.New("malformed request line")
	}

	fast.auth, fast.gitProtocol = "", ""
	header := make(http.Header, 16)
	var contentLength int64 = -1
	chunked := false
	closeConn := proto == "HTTP/1.0"
	wantContinue := false
	for {
		hline, err := readCRLFLine(br, scratch)
		if err != nil {
			return nil, nil, err
		}
		if len(hline) == 0 {
			break
		}
		colon := bytes.IndexByte(hline, ':')
		if colon <= 0 {
			continue
		}
		name := textproto.CanonicalMIMEHeaderKey(string(hline[:colon]))
		value := strings.TrimSpace(string(hline[colon+1:]))
		switch name {
		case "Content-Length":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				contentLength = n
			}
		case "Transfer-Encoding":
			if strings.Contains(strings.ToLower(value), "chunked") {
				chunked = true
			}
		case "Connection":
			lower := strings.ToLower(value)
			if strings.Contains(lower, "close") {
				closeConn = true
			} else if strings.Contains(lower, "keep-alive") {
				closeConn = false
			}
		case "Expect":
			if strings.Contains(strings.ToLower(value), "100-continue") {
				wantContinue = true
			}
		case "Authorization":
			fast.auth = value
		case "Git-Protocol":
			fast.gitProtocol = value
		}
		header[name] = append(header[name], value)
	}

	var body []byte
	if chunked {
		if wantContinue {
			_, _ = conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
		}
		body, err = readChunkedBody(br)
		if err != nil {
			return nil, nil, err
		}
		contentLength = int64(len(body))
	} else if contentLength > 0 {
		if contentLength > maxRequestBody {
			return nil, nil, errors.New("request body too large")
		}
		if wantContinue {
			_, _ = conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
		}
		body = make([]byte, contentLength)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil, nil, err
		}
	} else if contentLength == 0 {
		body = nil
	}

	reqURL, err := url.ParseRequestURI(target)
	if err != nil {
		reqURL = &url.URL{Path: target}
	}
	req := &http.Request{
		Method:        method,
		URL:           reqURL,
		Proto:         proto,
		ProtoMajor:    protoMajor(proto),
		ProtoMinor:    protoMinor(proto),
		Header:        header,
		Body:          &pReader{data: body},
		ContentLength: contentLength,
		Host:          header.Get("Host"),
		RequestURI:    target,
		RemoteAddr:    conn.RemoteAddr().String(),
		Close:         closeConn,
	}
	return req, body, nil
}

func protoMajor(proto string) int {
	if proto == "HTTP/1.0" {
		return 1
	}
	return 1
}

func protoMinor(proto string) int {
	if proto == "HTTP/1.0" {
		return 0
	}
	return 1
}

func splitRequestLine(line []byte) (method, target, proto string, ok bool) {
	first := bytes.IndexByte(line, ' ')
	if first <= 0 {
		return "", "", "", false
	}
	last := bytes.LastIndexByte(line, ' ')
	if last <= first {
		return "", "", "", false
	}
	return string(line[:first]), string(line[first+1 : last]), string(line[last+1:]), true
}

// readCRLFLine reads one line up to and including '\n', returning it without the trailing CRLF.
func readCRLFLine(br *bufio.Reader, scratch *[]byte) ([]byte, error) {
	*scratch = (*scratch)[:0]
	for {
		chunk, err := br.ReadSlice('\n')
		*scratch = append(*scratch, chunk...)
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(*scratch) > 1<<20 {
				return nil, errors.New("header line too long")
			}
			continue
		}
		return nil, err
	}
	out := *scratch
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	if n := len(out); n > 0 && out[n-1] == '\r' {
		out = out[:n-1]
	}
	return out, nil
}

func readChunkedBody(br *bufio.Reader) ([]byte, error) {
	var body []byte
	scratch := make([]byte, 0, 64)
	for {
		line, err := readCRLFLine(br, &scratch)
		if err != nil {
			return nil, err
		}
		if semi := bytes.IndexByte(line, ';'); semi >= 0 {
			line = line[:semi]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(string(line)), 16, 64)
		if err != nil {
			return nil, errors.New("bad chunk size")
		}
		if size == 0 {
			for {
				trailer, err := readCRLFLine(br, &scratch)
				if err != nil {
					return nil, err
				}
				if len(trailer) == 0 {
					break
				}
			}
			return body, nil
		}
		if int64(len(body))+size > maxRequestBody {
			return nil, errors.New("request body too large")
		}
		start := len(body)
		n := int(size)
		body = append(body, make([]byte, n+2)...)
		if _, err := io.ReadFull(br, body[start:start+n+2]); err != nil {
			return nil, err
		}
		body = body[:start+n]
	}
}

// fastWriter collects one reply in memory so it can go out as a single write.
type fastWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *fastWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header, 8)
	}
	return w.header
}

func (w *fastWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *fastWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func (w *fastWriter) Flush() {}

// writeFastResponse writes the status line, the headers and the body in one buffered pass.
func writeFastResponse(conn net.Conn, bw *bufio.Writer, req *http.Request, w *fastWriter) error {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	body := w.body.Bytes()
	noBody := req.Method == "HEAD" || status == 204 || status == 304

	_ = conn.SetWriteDeadline(time.Now().Add(fastIdleTimeout))
	bw.WriteString("HTTP/1.1 ")
	bw.WriteString(strconv.Itoa(status))
	bw.WriteByte(' ')
	bw.WriteString(statusText(status))
	bw.WriteString("\r\n")
	if w.header != nil {
		for name, values := range w.header {
			if strings.EqualFold(name, "Content-Length") {
				continue
			}
			for _, value := range values {
				bw.WriteString(name)
				bw.WriteString(": ")
				bw.WriteString(value)
				bw.WriteString("\r\n")
			}
		}
	}
	if w.header.Get("Date") == "" {
		bw.WriteString("Date: ")
		bw.WriteString(time.Now().UTC().Format(http.TimeFormat))
		bw.WriteString("\r\n")
	}
	if !noBody {
		bw.WriteString("Content-Length: ")
		bw.WriteString(strconv.Itoa(len(body)))
		bw.WriteString("\r\n")
	} else if req.Method == "HEAD" {
		bw.WriteString("Content-Length: ")
		bw.WriteString(strconv.Itoa(len(body)))
		bw.WriteString("\r\n")
	}
	bw.WriteString("\r\n")
	if !noBody && len(body) > 0 {
		bw.Write(body)
	}
	return bw.Flush()
}

func statusText(code int) string {
	if text := http.StatusText(code); text != "" {
		return text
	}
	return "Status"
}