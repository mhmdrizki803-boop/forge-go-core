package main

// Git smart-HTTP protocol support: pkt-lines, service advertisements, upload and
// receive negotiation, plus native packfile encoding and decoding.

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

func hexEncode(b []byte) string { return hex.EncodeToString(b) }

func unhex(s string) []byte {
	out, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return out
}

func sha1Bytes(b []byte) []byte {
	sum := sha1.Sum(b)
	return sum[:]
}

// ZeroOid is the all-zero object id: an empty ref, a delete, a creation.
const ZeroOid = "0000000000000000000000000000000000000000"

// ReceiveCaps are the capabilities every receive-pack advertisement claims.
const ReceiveCaps = "report-status side-band-64k delete-refs ofs-delta object-format=sha1 agent=openforge/1.0"

// ---------------------------------------------------------------- pkt-lines

func pktLine(s string) string {
	if s == "" {
		return "0000"
	}
	length := 4 + len([]byte(s))
	return fmt.Sprintf("%04x", length) + s
}

func pktLineBuf(payload []byte) []byte {
	head := []byte(fmt.Sprintf("%04x", 4+len(payload)))
	return append(head, payload...)
}

type Pkt struct {
	Kind string
	Data []byte
}

func parsePktLines(buf []byte) ([]Pkt, error) {
	out := []Pkt{}
	at := 0
	for at+4 <= len(buf) {
		header := string(buf[at : at+4])
		length, err := strconv.ParseUint(header, 16, 32)
		if err != nil {
			return nil, fmt.Errorf("bad pkt-line length: %q", header)
		}
		switch {
		case length == 0:
			out = append(out, Pkt{Kind: "flush"})
			at += 4
		case length == 1:
			out = append(out, Pkt{Kind: "delim"})
			at += 4
		case length < 4:
			return nil, fmt.Errorf("pkt-line length under 4: %d", length)
		default:
			if at+int(length) > len(buf) {
				return nil, errors.New("pkt-line runs past the end of the buffer")
			}
			out = append(out, Pkt{Kind: "data", Data: buf[at+4 : at+int(length)]})
			at += int(length)
		}
	}
	return out, nil
}

// pktLines returns every data pkt-line as a string with its line ending stripped.
func pktLines(buf []byte) []string {
	pkts, err := parsePktLines(buf)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(pkts))
	for _, pkt := range pkts {
		if pkt.Kind != "data" {
			continue
		}
		out = append(out, strings.TrimRight(string(pkt.Data), "\r\n"))
	}
	return out
}

type PushCommand struct {
	Old  string
	Next string
	Ref  string
	Caps string
}

func readCommands(buf []byte) ([]PushCommand, int) {
	commands := []PushCommand{}
	at := 0
	for at+4 <= len(buf) {
		header := string(buf[at : at+4])
		length, err := strconv.ParseUint(header, 16, 32)
		if err != nil {
			break
		}
		if length == 0 {
			at += 4
			break
		}
		if length < 4 {
			break
		}
		if at+int(length) > len(buf) {
			break
		}
		data := buf[at+4 : at+int(length)]
		text := string(data)
		nul := strings.IndexByte(text, 0)
		head := text
		caps := ""
		if nul >= 0 {
			head = text[:nul]
			caps = text[nul+1:]
		}
		head = strings.TrimRight(head, "\r\n")
		parts := strings.Split(strings.TrimSpace(head), " ")
		if len(parts) >= 3 {
			commands = append(commands, PushCommand{Old: parts[0], Next: parts[1], Ref: parts[2], Caps: caps})
		}
		at += int(length)
	}
	return commands, at
}

// ---------------------------------------------------------------- advertisement

var advertiseCache = map[string][]byte{}
var lsRefsCache = map[string][]byte{}

func advertiseRefs(store *Store, repo *RepoRecord, service string) []byte {
	key := repo.FullName + "|" + service + "|" + strconv.Itoa(store.Writes)
	if cached, ok := advertiseCache[key]; ok {
		return cached
	}
	built := buildAdvertisement(store, repo, service)
	if len(advertiseCache) > 256 {
		advertiseCache = map[string][]byte{}
	}
	advertiseCache[key] = built
	return built
}

func buildAdvertisement(store *Store, repo *RepoRecord, service string) []byte {
	var parts []byte
	parts = append(parts, []byte(pktLine("# service="+service+"\n"))...)
	parts = append(parts, []byte("0000")...)

	if service == "git-upload-pack" {
		for _, line := range []string{"version 2", "agent=openforge/1.0", "ls-refs", "fetch=shallow", "object-format=sha1"} {
			parts = append(parts, []byte(pktLine(line+"\n"))...)
		}
		parts = append(parts, []byte("0000")...)
		return parts
	}

	refs := store.ListRefs(repo, "")
	if len(refs) == 0 {
		parts = append(parts, []byte(pktLine(ZeroOid+" capabilities^{}\x00"+ReceiveCaps+"\n"))...)
	} else {
		parts = append(parts, []byte(pktLine(refs[0][1]+" "+refs[0][0]+"\x00"+ReceiveCaps+"\n"))...)
		for _, pair := range refs[1:] {
			parts = append(parts, []byte(pktLine(pair[1]+" "+pair[0]+"\n"))...)
		}
	}
	parts = append(parts, []byte("0000")...)
	return parts
}

// ---------------------------------------------------------------- object walking

var typeCode = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

// reachable returns every object id reachable from oid, optionally depth-limited.
func reachable(store *Store, repo *RepoRecord, oid string, depth int) []string {
	out := []string{}
	seen := map[string]bool{}
	type item struct {
		oid   string
		level int
	}
	stack := []item{{oid: oid}}
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[top.oid] {
			continue
		}
		object := store.GetObject(repo, top.oid)
		if object == nil {
			continue
		}
		seen[top.oid] = true
		out = append(out, top.oid)
		switch object.Type {
		case "tree":
			body := object.Body
			at := 0
			for at+20 <= len(body) {
				nul := indexByteFrom(body, 0, at)
				if nul < 0 || nul+21 > len(body) {
					break
				}
				stack = append(stack, item{oid: hexEncode(body[nul+1 : nul+21]), level: top.level})
				at = nul + 21
			}
		case "commit":
			for _, line := range strings.Split(string(object.Body), "\n") {
				if strings.HasPrefix(line, "tree ") {
					stack = append(stack, item{oid: strings.TrimSpace(line[5:]), level: top.level})
				} else if strings.HasPrefix(line, "parent ") {
					if depth == 0 || top.level+1 < depth {
						stack = append(stack, item{oid: strings.TrimSpace(line[7:]), level: top.level + 1})
					}
				} else if strings.HasPrefix(line, "object ") {
					stack = append(stack, item{oid: strings.TrimSpace(line[7:]), level: top.level})
				} else if line == "" {
					break
				}
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- packs: writing

func objectHeader(typ, size int) []byte {
	out := []byte{}
	left := size
	first := byte((typ << 4) | (left & 0x0f))
	left >>= 4
	for left > 0 {
		out = append(out, first|0x80)
		first = byte(left & 0x7f)
		left >>= 7
	}
	out = append(out, first)
	return out
}

func deflateBytes(body []byte) []byte {
	var buf bytes.Buffer
	writer := zlib.NewWriter(&buf)
	writer.Write(body)
	writer.Close()
	return buf.Bytes()
}

func packObject(typ string, body []byte) []byte {
	code := typeCode[typ]
	return append(objectHeader(code, len(body)), deflateBytes(body)...)
}

var packCache = map[string][]byte{}

const packCacheMax = 256

func packKey(repo *RepoRecord, wants []string, depth int) string {
	sorted := make([]string, len(wants))
	copy(sorted, wants)
	sort.Strings(sorted)
	return repo.FullName + "|" + strings.Join(sorted, ",") + "|" + strconv.Itoa(depth) + "|" +
		strconv.Itoa(len(repo.Data.Objects)) + "|" + strconv.Itoa(len(repo.Data.Refs))
}

func packFor(store *Store, repo *RepoRecord, wants, haves []string, depth int) []byte {
	key := ""
	if len(haves) == 0 {
		key = packKey(repo, wants, depth)
		if hit, ok := packCache[key]; ok {
			return hit
		}
	}
	objects := collectObjects(store, repo, wants, haves, depth)
	pack := writePack(store, repo, objects)
	if key != "" {
		if len(packCache) >= packCacheMax {
			for existing := range packCache {
				delete(packCache, existing)
				break
			}
		}
		packCache[key] = pack
	}
	return pack
}

func writePack(store *Store, repo *RepoRecord, oids []string) []byte {
	header := make([]byte, 12)
	copy(header, []byte("PACK"))
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	n := len(oids)
	header[8] = byte(n >> 24)
	header[9] = byte(n >> 16)
	header[10] = byte(n >> 8)
	header[11] = byte(n)
	parts := [][]byte{header}
	for _, oid := range oids {
		object := store.GetObject(repo, oid)
		if object == nil {
			continue
		}
		parts = append(parts, packObject(object.Type, object.Body))
	}
	body := bytes.Join(parts, nil)
	return append(body, sha1Bytes(body)...)
}

func verifyPackChecksum(buf []byte) bool {
	if len(buf) < 32 {
		return false
	}
	body := buf[:len(buf)-20]
	return bytes.Equal(sha1Bytes(body), buf[len(buf)-20:])
}

// ---------------------------------------------------------------- packs: reading

type PackRecord struct {
	Type string
	Body []byte
	Oid  string
}

type byteReader struct {
	b []byte
	i int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

func (r *byteReader) ReadByte() (byte, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	b := r.b[r.i]
	r.i++
	return b, nil
}

// zlibInflate inflates one zlib stream and reports how many input bytes it consumed.
func zlibInflate(buf []byte) ([]byte, int, error) {
	if len(buf) < 2 {
		return nil, 0, errors.New("zlib stream too short")
	}
	cmf := buf[0]
	flg := buf[1]
	if cmf&0x0f != 8 {
		return nil, 0, errors.New("bad zlib compression method")
	}
	if (uint16(cmf)<<8|uint16(flg))%31 != 0 {
		return nil, 0, errors.New("bad zlib header check")
	}
	start := 2
	if flg&0x20 != 0 {
		start += 4
	}
	if len(buf) < start {
		return nil, 0, errors.New("zlib stream truncated")
	}
	reader := &byteReader{b: buf[start:]}
	flateReader := flate.NewReader(reader)
	out, err := io.ReadAll(flateReader)
	flateReader.Close()
	// The deflate stream is followed by the 4-byte adler32 that closes the zlib stream; go's
	// flate stops at the end of the deflate data, so the trailer is counted here.
	consumed := start + reader.i + 4
	if consumed > len(buf) {
		consumed = len(buf)
	}
	if err != nil {
		return out, consumed, err
	}
	return out, consumed, nil
}

func DEBUG_PACK(format string, args ...any) {
	if os.Getenv("FORGE_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[pack] "+format+"\n", args...)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func readVarint(buf []byte, at int) (int, int, error) {
	value := 0
	shift := 0
	index := at
	for {
		if index >= len(buf) {
			return 0, index, errors.New("truncated varint")
		}
		b := buf[index]
		index++
		value |= int(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			break
		}
	}
	return value, index, nil
}

// applyDelta applies a git delta to base.
func applyDelta(base, delta []byte) ([]byte, error) {
	at := 0
	baseSize, next, err := readVarint(delta, at)
	if err != nil {
		return nil, err
	}
	at = next
	resultSize, next, err := readVarint(delta, at)
	if err != nil {
		return nil, err
	}
	at = next
	if baseSize != len(base) {
		return nil, fmt.Errorf("delta base size %d does not match %d", baseSize, len(base))
	}
	out := []byte{}
	for at < len(delta) {
		op := delta[at]
		at++
		if op&0x80 != 0 {
			offset := 0
			size := 0
			if op&0x01 != 0 {
				offset |= int(delta[at])
				at++
			}
			if op&0x02 != 0 {
				offset |= int(delta[at]) << 8
				at++
			}
			if op&0x04 != 0 {
				offset |= int(delta[at]) << 16
				at++
			}
			if op&0x08 != 0 {
				offset |= int(delta[at]) << 24
				at++
			}
			if op&0x10 != 0 {
				size |= int(delta[at])
				at++
			}
			if op&0x20 != 0 {
				size |= int(delta[at]) << 8
				at++
			}
			if op&0x40 != 0 {
				size |= int(delta[at]) << 16
				at++
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > len(base) {
				return nil, errors.New("delta copy runs past the base")
			}
			out = append(out, base[offset:offset+size]...)
		} else if op != 0 {
			if at+int(op) > len(delta) {
				return nil, errors.New("delta insert runs past the delta")
			}
			out = append(out, delta[at:at+int(op)]...)
			at += int(op)
		} else {
			return nil, errors.New("delta op 0 is reserved")
		}
	}
	if len(out) != resultSize {
		return nil, fmt.Errorf("delta produced %d bytes, expected %d", len(out), resultSize)
	}
	return out, nil
}

type pendingDelta struct {
	offset     int
	record     PackRecord
	baseOffset int
	baseOid    string
	byOffset   bool
}

// readPack parses a packfile into fully resolved objects.
func readPack(buf []byte) ([]PackRecord, error) {
	if len(buf) < 32 {
		return nil, errors.New("pack too short")
	}
	if string(buf[0:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	version := int(buf[4])<<24 | int(buf[5])<<16 | int(buf[6])<<8 | int(buf[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(buf[8])<<24 | int(buf[9])<<16 | int(buf[10])<<8 | int(buf[11])
	DEBUG_PACK("version=%d count=%d len=%d", version, count, len(buf))

	byOffset := map[int]PackRecord{}
	byOid := map[string]PackRecord{}
	pending := []pendingDelta{}
	ordered := []PackRecord{}

	at := 12
	for index := 0; index < count; index++ {
		start := at
		if at >= len(buf) {
			return nil, errors.New("pack truncated in a record header")
		}
		b := buf[at]
		at++
		typ := (b >> 4) & 0x07
		size := int(b & 0x0f)
		shift := 4
		for b&0x80 != 0 {
			if at >= len(buf) {
				return nil, errors.New("pack truncated in a record size")
			}
			b = buf[at]
			at++
			size |= int(b&0x7f) << shift
			shift += 7
		}

		if typ == 6 {
			DEBUG_PACK("ofs-delta at=%d", at)
			if at >= len(buf) {
				return nil, errors.New("pack truncated in an ofs-delta")
			}
			c := buf[at]
			at++
			offset := int(c & 0x7f)
			for c&0x80 != 0 {
				if at >= len(buf) {
					return nil, errors.New("pack truncated in an ofs-delta")
				}
				c = buf[at]
				at++
				offset = ((offset + 1) << 7) | int(c&0x7f)
			}
			delta, consumed, err := zlibInflate(buf[at:])
			if err != nil {
				DEBUG_PACK("ofs inflate failed at=%d next=%x", at, buf[at:min(at+4, len(buf))])
				return nil, err
			}
			DEBUG_PACK("ofs-delta baseOffset=%d consumed=%d", start-offset, consumed)
			at += consumed
			pending = append(pending, pendingDelta{offset: start, record: PackRecord{Type: "blob", Body: delta}, baseOffset: start - offset, byOffset: true})
			continue
		}

		if typ == 7 {
			DEBUG_PACK("ref-delta at=%d", at)
			if at+20 > len(buf) {
				return nil, errors.New("pack truncated in a ref-delta")
			}
			baseOid := hexEncode(buf[at : at+20])
			at += 20
			delta, consumed, err := zlibInflate(buf[at:])
			if err != nil {
				return nil, err
			}
			at += consumed
			pending = append(pending, pendingDelta{offset: start, record: PackRecord{Type: "blob", Body: delta}, baseOid: baseOid})
			continue
		}

		objectType, ok := codeType[int(typ)]
		if !ok {
			DEBUG_PACK("unknown type=%d at=%d start=%d index=%d len=%d first=%x", typ, at, start, index, len(buf), buf[:min(16, len(buf))])
			return nil, fmt.Errorf("unknown pack object type %d", typ)
		}
		body, consumed, err := zlibInflate(buf[at:])
		if err != nil {
			DEBUG_PACK("inflate failed type=%d at=%d: %v", typ, at, err)
			return nil, err
		}
		DEBUG_PACK("obj type=%d size=%d at=%d consumed=%d bodylen=%d", typ, size, at, consumed, len(body))
		at += consumed
		if len(body) != size {
			return nil, fmt.Errorf("pack record size %d but inflated %d", size, len(body))
		}
		record := PackRecord{Type: objectType, Body: body, Oid: hashObject(objectType, body)}
		byOffset[start] = record
		byOid[record.Oid] = record
		ordered = append(ordered, record)
	}

	leftover := pending
	for pass := 0; pass < 64 && len(leftover) > 0; pass++ {
		next := []pendingDelta{}
		for _, item := range leftover {
			var base PackRecord
			found := false
			if item.byOffset {
				base, found = byOffset[item.baseOffset]
			} else {
				base, found = byOid[item.baseOid]
			}
			if !found {
				next = append(next, item)
				continue
			}
			body, err := applyDelta(base.Body, item.record.Body)
			if err != nil {
				return nil, err
			}
			record := PackRecord{Type: base.Type, Body: body, Oid: hashObject(base.Type, body)}
			item.record = record
			byOffset[item.offset] = record
			byOid[record.Oid] = record
			ordered = append(ordered, record)
		}
		if len(next) == len(leftover) {
			break
		}
		leftover = next
	}
	if len(leftover) > 0 {
		return nil, fmt.Errorf("unresolved delta objects: %d", len(leftover))
	}
	return ordered, nil
}

// ---------------------------------------------------------------- upload-pack

func peelToCommit(store *Store, repo *RepoRecord, oid string) (string, bool) {
	at := oid
	for hop := 0; hop < 8; hop++ {
		object := store.GetObject(repo, at)
		if object == nil {
			return "", false
		}
		if object.Type == "commit" {
			return at, hop != 0
		}
		if object.Type != "tag" {
			return "", false
		}
		found := ""
		for _, line := range strings.Split(string(object.Body), "\n") {
			if strings.HasPrefix(line, "object ") {
				found = strings.TrimSpace(line[7:])
				break
			}
		}
		if found == "" {
			return "", false
		}
		at = found
	}
	return "", false
}

func shallowBoundary(store *Store, repo *RepoRecord, wants []string, depth int) []string {
	out := []string{}
	seenOut := map[string]bool{}
	for _, want := range wants {
		type item struct {
			oid   string
			level int
		}
		queue := []item{{oid: want}}
		seen := map[string]bool{}
		for len(queue) > 0 {
			top := queue[0]
			queue = queue[1:]
			if seen[top.oid] {
				continue
			}
			seen[top.oid] = true
			object := store.GetObject(repo, top.oid)
			if object == nil || object.Type != "commit" {
				continue
			}
			if top.level+1 == depth {
				if !seenOut[top.oid] {
					seenOut[top.oid] = true
					out = append(out, top.oid)
				}
				continue
			}
			for _, line := range strings.Split(string(object.Body), "\n") {
				if line == "" {
					break
				}
				if strings.HasPrefix(line, "parent ") {
					queue = append(queue, item{oid: strings.TrimSpace(line[7:]), level: top.level + 1})
				}
			}
		}
	}
	return out
}

func deepenOf(request []string) int {
	for _, line := range request {
		if strings.HasPrefix(line, "deepen ") {
			n, err := strconv.Atoi(strings.TrimSpace(line[7:]))
			if err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// collectObjects is the wants' closure minus the haves' closure.
func collectObjects(store *Store, repo *RepoRecord, wants, haves []string, depth int) []string {
	haveSet := map[string]bool{}
	haveDepth := 0
	if depth > 0 {
		haveDepth = depth - 1
	}
	for _, have := range haves {
		for _, oid := range reachable(store, repo, have, haveDepth) {
			haveSet[oid] = true
		}
	}
	out := []string{}
	seen := map[string]bool{}
	for _, want := range wants {
		for _, oid := range reachable(store, repo, want, depth) {
			if haveSet[oid] || seen[oid] {
				continue
			}
			seen[oid] = true
			out = append(out, oid)
		}
	}
	return out
}

// handleUploadPack answers an upload-pack request: the v2 ls-refs and fetch commands, or v0.
func handleUploadPack(store *Store, repo *RepoRecord, body []byte) []byte {
	request := pktLines(body)
	var parts []byte

	if len(request) == 0 {
		parts = append(parts, []byte(pktLine("NAK\n"))...)
		parts = append(parts, []byte("0000")...)
		return parts
	}

	isV2 := strings.HasPrefix(request[0], "command=")

	if isV2 {
		command := strings.TrimSpace(strings.TrimPrefix(request[0], "command="))
		flags := map[string]bool{}
		for _, line := range request[1:] {
			if !strings.Contains(line, " ") || strings.HasPrefix(line, "ref-prefix ") {
				flags[line] = true
			}
		}

		if command == "ls-refs" {
			lsKey := repo.FullName + "|ls|" + strconv.Itoa(store.Writes) + "|" + b64(string(body))
			if hit, ok := lsRefsCache[lsKey]; ok {
				return hit
			}
			refs := store.ListRefs(repo, "")
			head, hasHead := store.GetRefOk(repo, "refs/heads/"+repo.DefaultBranch)
			unborn := !hasHead
			symref := "symref=HEAD:refs/heads/" + repo.DefaultBranch
			prefixesKnown := false
			wantsHead := false
			for _, line := range request {
				if strings.HasPrefix(line, "ref-prefix ") {
					prefixesKnown = true
					if strings.HasPrefix("HEAD", strings.TrimPrefix(line, "ref-prefix ")) {
						wantsHead = true
					}
				}
			}
			// A client that names no namespace wants HEAD; a client that asks for it by name
			// (git sends `ref-prefix HEAD`) gets it too. A namespace prefix keeps it out.
			showHead := !prefixesKnown || wantsHead
			switch {
			case !showHead:
				// no HEAD line
			case unborn:
				if flags["unborn"] || flags["symrefs"] {
					if flags["symrefs"] {
						parts = append(parts, []byte(pktLine("unborn HEAD "+symref+"\n"))...)
					} else {
						parts = append(parts, []byte(pktLine("unborn HEAD\n"))...)
					}
				}
			case flags["symrefs"]:
				parts = append(parts, []byte(pktLine(head+" HEAD "+symref+" symref-target:refs/heads/"+repo.DefaultBranch+"\n"))...)
			default:
				parts = append(parts, []byte(pktLine(head+" HEAD\n"))...)
			}
			prefixes := []string{}
			for _, line := range request {
				if strings.HasPrefix(line, "ref-prefix ") {
					prefixes = append(prefixes, strings.TrimPrefix(line, "ref-prefix "))
				}
			}
			wantPeel := flags["peel"]
			for _, pair := range refs {
				if len(prefixes) > 0 {
					matched := false
					for _, prefix := range prefixes {
						if strings.HasPrefix(pair[0], prefix) {
							matched = true
							break
						}
					}
					if !matched {
						continue
					}
				}
				attrs := ""
				if wantPeel {
					if peeled, ok := peelToCommit(store, repo, pair[1]); ok && peeled != "" {
						attrs = " peeled:" + peeled
					}
				}
				parts = append(parts, []byte(pktLine(pair[1]+" "+pair[0]+attrs+"\n"))...)
			}
			parts = append(parts, []byte("0000")...)
			if len(lsRefsCache) > 256 {
				lsRefsCache = map[string][]byte{}
			}
			lsRefsCache[lsKey] = parts
			return parts
		}

		wants := []string{}
		haves := []string{}
		for _, line := range request {
			if strings.HasPrefix(line, "want ") {
				wants = append(wants, strings.Fields(line[5:])[0])
			} else if strings.HasPrefix(line, "have ") {
				haves = append(haves, strings.Fields(line[5:])[0])
			}
		}
		deepen := deepenOf(request)
		pack := packFor(store, repo, wants, haves, deepen)
		// A client that sent haves is negotiating and reads an acknowledgments section first. It
		// asked for a pack with its `done`, so a NAK then the pack answers it whole; a client still
		// negotiating gets `ready` and comes back with its done.
		if len(haves) > 0 {
			parts = append(parts, []byte(pktLine("acknowledgments\n"))...)
			parts = append(parts, []byte(pktLine("ready\n"))...)
			parts = append(parts, []byte("0001")...)
		}
		if deepen > 0 {
			parts = append(parts, []byte(pktLine("shallow-info\n"))...)
			for _, oid := range shallowBoundary(store, repo, wants, deepen) {
				parts = append(parts, []byte(pktLine("shallow "+oid+"\n"))...)
			}
			parts = append(parts, []byte("0001")...)
		}
		parts = append(parts, []byte(pktLine("packfile\n"))...)
		for at := 0; at < len(pack); at += 65515 {
			end := at + 65515
			if end > len(pack) {
				end = len(pack)
			}
			payload := append([]byte{1}, pack[at:end]...)
			parts = append(parts, pktLineBuf(payload)...)
		}
		parts = append(parts, []byte("0000")...)
		return parts
	}

	if strings.HasPrefix(request[0], "want ") {
		wants := []string{}
		haves := []string{}
		for _, line := range request {
			if strings.HasPrefix(line, "want ") {
				wants = append(wants, strings.Fields(line[5:])[0])
			} else if strings.HasPrefix(line, "have ") {
				haves = append(haves, strings.Fields(line[5:])[0])
			}
		}
		pack := packFor(store, repo, wants, haves, deepenOf(request))
		parts = append(parts, []byte(pktLine("NAK\n"))...)
		parts = append(parts, pack...)
		return parts
	}

	parts = append(parts, []byte(pktLine("NAK\n"))...)
	parts = append(parts, []byte("0000")...)
	return parts
}

// ---------------------------------------------------------------- receive-pack

func isFastForward(store *Store, repo *RepoRecord, older, newer string) bool {
	if older == ZeroOid || older == newer {
		return true
	}
	seen := map[string]bool{}
	stack := []string{newer}
	for len(stack) > 0 {
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if next == older {
			return true
		}
		if seen[next] {
			continue
		}
		seen[next] = true
		object := store.GetObject(repo, next)
		if object == nil || object.Type != "commit" {
			continue
		}
		for _, line := range strings.Split(string(object.Body), "\n") {
			if line == "" {
				break
			}
			if strings.HasPrefix(line, "parent ") {
				stack = append(stack, strings.TrimSpace(line[7:]))
			}
		}
	}
	return false
}

// handleReceivePack applies a receive-pack request and reports status per ref.
func handleReceivePack(store *Store, repo *RepoRecord, body []byte) []byte {
	commands, packStart := readCommands(body)
	sawPack := false

	if packStart < len(body) && packStart+4 <= len(body) && string(body[packStart:packStart+4]) == "PACK" {
		packBuf := body[packStart:]
		sawPack = true
		DEBUG_PACK("packStart=%d len=%d magic=%q", packStart, len(packBuf), string(packBuf[:min(4, len(packBuf))]))
		if !verifyPackChecksum(packBuf) {
			return receivePackError("pack checksum mismatch")
		}
		records, err := readPack(packBuf)
		if err != nil {
			DEBUG_PACK("readPack failed: %v", err)
			return receivePackError(err.Error())
		}
		for _, record := range records {
			store.PutObject(repo, record.Type, record.Body)
		}
	}

	wantSideBand := false
	for _, command := range commands {
		if strings.Contains(command.Caps, "side-band-64k") {
			wantSideBand = true
		}
	}

	type verdict struct {
		command PushCommand
		ok      bool
		reason  string
	}
	accepted := []verdict{}
	for _, command := range commands {
		isDelete := command.Next == ZeroOid
		current, hasCurrent := store.GetRefOk(repo, command.Ref)
		if !strings.HasPrefix(command.Ref, "refs/") {
			accepted = append(accepted, verdict{command: command, ok: false, reason: "funny refname"})
			continue
		}
		if isDelete {
			accepted = append(accepted, verdict{command: command, ok: command.Old == ZeroOid || (hasCurrent && command.Old == current)})
			continue
		}
		if !store.HasObject(repo, command.Next) {
			accepted = append(accepted, verdict{command: command, ok: false, reason: "missing object"})
			continue
		}
		older := ZeroOid
		if hasCurrent {
			older = current
		}
		if !isFastForward(store, repo, older, command.Next) {
			accepted = append(accepted, verdict{command: command, ok: false, reason: "non-fast-forward"})
			continue
		}
		if hasCurrent && command.Old != ZeroOid && command.Old != current {
			accepted = append(accepted, verdict{command: command, ok: false, reason: "stale info"})
			continue
		}
		accepted = append(accepted, verdict{command: command, ok: true})
	}

	report := []string{}
	for _, item := range accepted {
		if !item.ok {
			words := strings.Fields(item.reason)
			reason := "rejected"
			if len(words) > 0 {
				reason = words[0]
			}
			report = append(report, "ng "+item.command.Ref+" "+reason)
			continue
		}
		if item.command.Next == ZeroOid {
			store.DeleteRef(repo, item.command.Ref)
		} else {
			store.SetRef(repo, item.command.Ref, item.command.Next)
		}
		repo.PushedAt = EPOCH
		report = append(report, "ok "+item.command.Ref)
	}

	status := []string{"unpack ok"}
	status = append(status, report...)
	reportFramed := []byte{}
	for _, line := range status {
		reportFramed = append(reportFramed, []byte(pktLine(line+"\n"))...)
	}
	reportFramed = append(reportFramed, []byte("0000")...)

	parts := []byte{}
	if wantSideBand {
		count := 0
		if sawPack {
			count = 1
		}
		progress := []byte(fmt.Sprintf("Counting objects: %d, done.\n", count))
		parts = append(parts, pktLineBuf(append([]byte{2}, progress...))...)
		for at := 0; at < len(reportFramed); at += 65515 {
			end := at + 65515
			if end > len(reportFramed) {
				end = len(reportFramed)
			}
			parts = append(parts, pktLineBuf(append([]byte{1}, reportFramed[at:end]...))...)
		}
	} else {
		parts = append(parts, reportFramed...)
	}
	parts = append(parts, []byte("0000")...)
	return parts
}

// receivePackError answers a fatal unpack failure in the report's own shape.
func receivePackError(message string) []byte {
	framed := []byte{}
	for _, line := range []string{"unpack " + message} {
		framed = append(framed, []byte(pktLine(line+"\n"))...)
	}
	framed = append(framed, []byte("0000")...)
	parts := pktLineBuf(append([]byte{1}, framed...))
	return append(parts, []byte("0000")...)
}
