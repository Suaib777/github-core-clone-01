// gitcore.go — the git object layer: SHA-1, zlib, pkt-lines, packfiles, trees,
// commits, tags, reachability, merge-base and tree diffs. Pure functions over an
// object lookup; no I/O. Ported from the TypeScript forge (gitcore.ts), keeping
// the wire behaviour identical.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const ZeroOID = "0000000000000000000000000000000000000000"

type GitObjectType string

const (
	ObjBlob   GitObjectType = "blob"
	ObjTree   GitObjectType = "tree"
	ObjCommit GitObjectType = "commit"
	ObjTag    GitObjectType = "tag"
)

type GitObject struct {
	Type GitObjectType
	Data []byte
}

type ObjectLookup func(oid string) *GitObject

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// HashObject returns the id of an object: SHA-1 over "<type> <size>\0<data>".
func HashObject(typ GitObjectType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", typ, len(data))))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func deflate(data []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	_, _ = w.Write(data)
	_ = w.Close()
	return buf.Bytes()
}

func inflate(data []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// ---------------------------------------------------------------------------
// pkt-lines
// ---------------------------------------------------------------------------

type PktKind int

const (
	PktData PktKind = iota
	PktFlush
	PktDelim
)

type Pkt struct {
	Kind    PktKind
	Payload []byte
}

// PktLine encodes one pkt-line: 4 hex digits of (payload length + 4), then payload.
func PktLine(payload []byte) []byte {
	n := len(payload) + 4
	if n > 0xffff {
		panic(fmt.Sprintf("pkt-line of %d bytes exceeds the 65535 limit", n))
	}
	out := make([]byte, 0, n)
	out = append(out, []byte(fmt.Sprintf("%04x", n))...)
	out = append(out, payload...)
	return out
}

func PktText(s string) []byte { return PktLine([]byte(s)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// EncodePktLines encodes several lines; "flush"/"delim" become control packets.
func EncodePktLines(lines [][]byte, controls []string) []byte {
	// lines and controls are parallel: controls[i] is "flush", "delim" or "".
	var buf bytes.Buffer
	for i, line := range lines {
		switch controls[i] {
		case "flush":
			buf.Write(pktFlush)
		case "delim":
			buf.Write(pktDelim)
		default:
			buf.Write(PktLine(line))
		}
	}
	return buf.Bytes()
}

// EncodePktStrings is EncodePktLines for string payloads.
func EncodePktStrings(lines []string, controls []string) []byte {
	raw := make([][]byte, len(lines))
	for i, s := range lines {
		raw[i] = []byte(s)
	}
	return EncodePktLines(raw, controls)
}

// DecodePktLines splits a buffer of pkt-lines into packets.
func DecodePktLines(buf []byte) ([]Pkt, error) {
	var out []Pkt
	off := 0
	for off < len(buf) {
		if off+4 > len(buf) {
			return nil, errors.New("pkt-line truncated at length prefix")
		}
		head := string(buf[off : off+4])
		if head == "0000" {
			out = append(out, Pkt{Kind: PktFlush})
			off += 4
			continue
		}
		if head == "0001" {
			out = append(out, Pkt{Kind: PktDelim})
			off += 4
			continue
		}
		length64, err := strconv.ParseInt(head, 16, 32)
		if err != nil {
			return nil, fmt.Errorf("malformed pkt-line length %q", head)
		}
		length := int(length64)
		if length < 4 || off+length > len(buf) {
			return nil, fmt.Errorf("malformed pkt-line length %q", head)
		}
		payload := make([]byte, length-4)
		copy(payload, buf[off+4:off+length])
		out = append(out, Pkt{Kind: PktData, Payload: payload})
		off += length
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// trees, commits, tags
// ---------------------------------------------------------------------------

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// ParseTree parses a tree object into its entries.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	off := 0
	for off < len(data) {
		space := bytes.IndexByte(data[off:], 0x20)
		if space < 0 {
			return nil, errors.New("tree entry has no mode")
		}
		mode := string(data[off : off+space])
		nul := bytes.IndexByte(data[off+space+1:], 0)
		if nul < 0 || off+space+1+nul+21 > len(data) {
			return nil, errors.New("tree entry has no name/oid")
		}
		nameStart := off + space + 1
		name := string(data[nameStart : nameStart+nul])
		oidStart := nameStart + nul + 1
		oid := hex.EncodeToString(data[oidStart : oidStart+20])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		off = oidStart + 20
	}
	return entries, nil
}

// treeSortKey is git's tree sort: entries sort by name, directories as if the
// name ended in '/'.
func treeSortKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

// BuildTree builds a tree object from entries (sorted git's way).
func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i]) < treeSortKey(sorted[j])
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode + " " + e.Name + "\x00")
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// ParseCommit parses a commit object.
func ParseCommit(data []byte) CommitInfo {
	text := string(data)
	split := strings.Index(text, "\n\n")
	head := text
	message := ""
	if split >= 0 {
		head = text[:split]
		message = text[split+2:]
	}
	var info CommitInfo
	info.Message = message
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			info.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			info.Parents = append(info.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			info.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			info.Committer = line[10:]
		}
	}
	return info
}

// BuildCommit builds a commit object.
func BuildCommit(info CommitInfo) []byte {
	var b strings.Builder
	b.WriteString("tree " + info.Tree + "\n")
	for _, p := range info.Parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + info.Author + "\n")
	b.WriteString("committer " + info.Committer + "\n")
	b.WriteString("\n")
	b.WriteString(info.Message)
	return []byte(b.String())
}

type TagInfo struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

// ParseTag parses an annotated tag object.
func ParseTag(data []byte) TagInfo {
	text := string(data)
	split := strings.Index(text, "\n\n")
	head := text
	message := ""
	if split >= 0 {
		head = text[:split]
		message = text[split+2:]
	}
	var info TagInfo
	info.Message = message
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			info.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			info.Type = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "tag "):
			info.Tag = line[4:]
		case strings.HasPrefix(line, "tagger "):
			info.Tagger = line[7:]
		}
	}
	return info
}

// BuildTag builds an annotated tag object.
func BuildTag(info TagInfo) []byte {
	return []byte(fmt.Sprintf("object %s\ntype %s\ntag %s\ntagger %s\n\n%s",
		info.Object, info.Type, info.Tag, info.Tagger, info.Message))
}

// PeelToCommit returns the commit an annotated tag peels to (following nested
// tags), or the oid itself.
func PeelToCommit(oid string, lookup ObjectLookup) string {
	current := oid
	for depth := 0; depth < 8; depth++ {
		obj := lookup(current)
		if obj == nil || obj.Type != ObjTag {
			return current
		}
		current = ParseTag(obj.Data).Object
	}
	return current
}

// ---------------------------------------------------------------------------
// packfiles (no deltas on write; deltas resolved on read)
// ---------------------------------------------------------------------------

var typeCode = map[GitObjectType]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]GitObjectType{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func packObjectHeader(typ GitObjectType, size int) []byte {
	code := typeCode[typ]
	var out []byte
	remaining := size
	first := (code << 4) | (remaining & 0x0f)
	remaining /= 16
	for remaining > 0 {
		out = append(out, byte(first|0x80))
		first = remaining & 0x7f
		remaining /= 128
	}
	out = append(out, byte(first))
	return out
}

// WritePack builds a packfile from plain objects (no deltas). Trailer is SHA-1
// over everything before it.
func WritePack(objects []GitObject) []byte {
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	header[8] = byte(len(objects) >> 24)
	header[9] = byte(len(objects) >> 16)
	header[10] = byte(len(objects) >> 8)
	header[11] = byte(len(objects))
	buf.Write(header)
	for _, obj := range objects {
		buf.Write(packObjectHeader(obj.Type, len(obj.Data)))
		buf.Write(deflate(obj.Data))
	}
	body := buf.Bytes()
	sum := sha1.Sum(body)
	return append(body, sum[:]...)
}

func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readVarint := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if off >= len(delta) {
				return 0, errors.New("delta varint truncated")
			}
			b := delta[off]
			off++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value, nil
	}
	baseSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if baseSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	resultSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	var chunks [][]byte
	total := 0
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			offset := 0
			size := 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy truncated")
					}
					offset |= int(delta[off]) << (8 * uint(i))
					off++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy truncated")
					}
					size |= int(delta[off]) << (8 * uint(i))
					off++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			chunks = append(chunks, base[offset:offset+size])
			total += size
		} else if op != 0 {
			if off+int(op) > len(delta) {
				return nil, errors.New("delta insert truncated")
			}
			chunks = append(chunks, delta[off:off+int(op)])
			total += int(op)
			off += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	out := bytes.Join(chunks, nil)
	if len(out) != resultSize || total != resultSize {
		return nil, errors.New("delta result size mismatch")
	}
	return out, nil
}

type PackRead struct {
	Objects    map[string]*GitObject
	DeltaCount int
}

// inflateAt inflates one zlib stream at offset; returns its bytes and the
// offset just past the stream.
func inflateAt(buf []byte, offset int) ([]byte, int, error) {
	r := bytes.NewReader(buf[offset:])
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, 0, err
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, 0, err
	}
	consumed := len(buf) - offset - r.Len()
	if consumed <= 0 {
		return nil, 0, fmt.Errorf("zlib stream at offset %d consumed no bytes", offset)
	}
	return data, offset + consumed, nil
}

// ReadPack reads a packfile (v2/v3), resolving ofs-delta and ref-delta entries
// against the pack itself (and resolveBase for external bases).
func ReadPack(pack []byte, resolveBase func(oid string) *GitObject) (*PackRead, error) {
	if len(pack) < 32 {
		return nil, errors.New("pack too short")
	}
	if string(pack[0:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailer := hex.EncodeToString(pack[len(pack)-20:])
	expected := sha1Hex(pack[:len(pack)-20])
	if trailer != expected {
		return nil, errors.New("pack trailer checksum mismatch")
	}
	objects := make(map[string]*GitObject)
	byOffset := make(map[int]*GitObject)
	type rawEntry struct {
		offset     int
		kind       int
		baseOffset int // -1 when none
		baseOID    string
		data       []byte
	}
	entries := make([]rawEntry, 0, count)
	deltaCount := 0
	off := 12
	end := len(pack) - 20
	for i := 0; i < count; i++ {
		if off >= end {
			return nil, errors.New("pack truncated")
		}
		entryOffset := off
		b := pack[off]
		off++
		kind := int(b>>4) & 0x7
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if off >= end {
				return nil, errors.New("pack truncated")
			}
			b = pack[off]
			off++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		baseOffset := -1
		baseOID := ""
		if kind == 6 {
			if off >= end {
				return nil, errors.New("pack truncated")
			}
			b = pack[off]
			off++
			offset := int(b & 0x7f)
			for b&0x80 != 0 {
				if off >= end {
					return nil, errors.New("pack truncated")
				}
				b = pack[off]
				off++
				offset = ((offset + 1) << 7) | int(b&0x7f)
			}
			baseOffset = entryOffset - offset
			deltaCount++
		} else if kind == 7 {
			if off+20 > end {
				return nil, errors.New("pack truncated")
			}
			baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
			deltaCount++
		} else if _, ok := codeType[kind]; !ok {
			return nil, fmt.Errorf("unknown pack object type %d", kind)
		}
		data, next, err := inflateAt(pack[:end], off)
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, errors.New("pack object size mismatch")
		}
		off = next
		entries = append(entries, rawEntry{offset: entryOffset, kind: kind, baseOffset: baseOffset, baseOID: baseOID, data: data})
	}
	for _, entry := range entries {
		var obj *GitObject
		if entry.kind == 6 || entry.kind == 7 {
			var base *GitObject
			if entry.baseOffset >= 0 {
				base = byOffset[entry.baseOffset]
			} else if entry.baseOID != "" {
				base = objects[entry.baseOID]
				if base == nil && resolveBase != nil {
					base = resolveBase(entry.baseOID)
				}
			}
			if base == nil {
				return nil, errors.New("delta base not found")
			}
			data, err := applyDelta(base.Data, entry.data)
			if err != nil {
				return nil, err
			}
			obj = &GitObject{Type: base.Type, Data: data}
		} else {
			obj = &GitObject{Type: codeType[entry.kind], Data: entry.data}
		}
		oid := HashObject(obj.Type, obj.Data)
		objects[oid] = obj
		byOffset[entry.offset] = obj
	}
	return &PackRead{Objects: objects, DeltaCount: deltaCount}, nil
}

// ---------------------------------------------------------------------------
// reachability, merge-base, tree diffs
// ---------------------------------------------------------------------------

// CollectReachable returns all objects reachable from tips.
func CollectReachable(tips []string, lookup ObjectLookup) map[string]bool {
	seen := make(map[string]bool)
	stack := append([]string(nil), tips...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		seen[oid] = true
		switch obj.Type {
		case ObjCommit:
			info := ParseCommit(obj.Data)
			stack = append(stack, info.Tree)
			stack = append(stack, info.Parents...)
		case ObjTree:
			entries, err := ParseTree(obj.Data)
			if err != nil {
				continue
			}
			for _, e := range entries {
				stack = append(stack, e.OID)
			}
		case ObjTag:
			stack = append(stack, ParseTag(obj.Data).Object)
		}
	}
	return seen
}

// IsAncestor reports whether ancestor is reachable from descendant by following
// commit parents.
func IsAncestor(ancestor, descendant string, lookup ObjectLookup) bool {
	if ancestor == descendant {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == ancestor {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		stack = append(stack, ParseCommit(obj.Data).Parents...)
	}
	return false
}

// MergeBase returns the nearest common ancestor commit of a and b (first found
// by a joint BFS), or "".
func MergeBase(a, b string, lookup ObjectLookup) string {
	if a == b {
		return a
	}
	aAncestors := make(map[string]bool)
	queueA := []string{a}
	for qi := 0; qi < len(queueA); qi++ {
		oid := queueA[qi]
		if aAncestors[oid] {
			continue
		}
		aAncestors[oid] = true
		obj := lookup(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		queueA = append(queueA, ParseCommit(obj.Data).Parents...)
	}
	queueB := []string{b}
	seenB := make(map[string]bool)
	for qi := 0; qi < len(queueB); qi++ {
		oid := queueB[qi]
		if aAncestors[oid] {
			return oid
		}
		if seenB[oid] {
			continue
		}
		seenB[oid] = true
		obj := lookup(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		queueB = append(queueB, ParseCommit(obj.Data).Parents...)
	}
	return ""
}

type FlatEntry struct {
	Mode string
	OID  string
}

// FlattenTree returns the flat file map of a tree: path -> {mode, oid}.
func FlattenTree(treeOID string, lookup ObjectLookup) map[string]FlatEntry {
	out := make(map[string]FlatEntry)
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		obj := lookup(oid)
		if obj == nil || obj.Type != ObjTree {
			return
		}
		entries, err := ParseTree(obj.Data)
		if err != nil {
			return
		}
		for _, e := range entries {
			path := e.Name
			if prefix != "" {
				path = prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				walk(e.OID, path)
			} else {
				out[path] = FlatEntry{Mode: e.Mode, OID: e.OID}
			}
		}
	}
	walk(treeOID, "")
	return out
}

// BuildTreeFromMap builds the tree objects for a flat file map. Returns the
// root tree oid; every tree object is registered through put.
func BuildTreeFromMap(files map[string]FlatEntry, put func(GitObjectType, []byte) string) string {
	type Node struct {
		files map[string]FlatEntry
		dirs  map[string]*Node
	}
	makeNode := func() *Node { return &Node{files: make(map[string]FlatEntry), dirs: make(map[string]*Node)} }
	root := makeNode()
	for path, entry := range files {
		segments := strings.Split(path, "/")
		node := root
		for i := 0; i < len(segments)-1; i++ {
			name := segments[i]
			child, ok := node.dirs[name]
			if !ok {
				child = makeNode()
				node.dirs[name] = child
			}
			node = child
		}
		node.files[segments[len(segments)-1]] = entry
	}
	var build func(node *Node) string
	build = func(node *Node) string {
		var entries []TreeEntry
		for name, entry := range node.files {
			entries = append(entries, TreeEntry{Mode: entry.Mode, Name: name, OID: entry.OID})
		}
		for name, child := range node.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		return put(ObjTree, BuildTree(entries))
	}
	return build(root)
}

type TreeChange struct {
	OldOID string // "" when added
	NewOID string // "" when removed
	HasOld bool
	HasNew bool
}

// DiffTrees returns the set of paths that differ between two trees.
func DiffTrees(baseTreeOID, headTreeOID string, lookup ObjectLookup) map[string]TreeChange {
	var base, head map[string]FlatEntry
	if baseTreeOID == ZeroOID {
		base = make(map[string]FlatEntry)
	} else {
		base = FlattenTree(baseTreeOID, lookup)
	}
	if headTreeOID == ZeroOID {
		head = make(map[string]FlatEntry)
	} else {
		head = FlattenTree(headTreeOID, lookup)
	}
	out := make(map[string]TreeChange)
	for path, entry := range head {
		if old, ok := base[path]; !ok || old.OID != entry.OID {
			ch := TreeChange{NewOID: entry.OID, HasNew: true}
			if ok {
				ch.OldOID = old.OID
				ch.HasOld = true
			}
			out[path] = ch
		}
	}
	for path, entry := range base {
		if _, ok := head[path]; !ok {
			out[path] = TreeChange{OldOID: entry.OID, HasOld: true}
		}
	}
	return out
}

// CommitGenerations returns commit generations from tips (0 = the tips).
// Stops expanding past maxDepth when given (maxDepth < 0 means no limit).
func CommitGenerations(tips []string, lookup ObjectLookup, maxDepth int) map[string]int {
	generations := make(map[string]int)
	type item struct {
		oid string
		gen int
	}
	queue := make([]item, 0, len(tips))
	for _, oid := range tips {
		queue = append(queue, item{oid, 0})
	}
	for qi := 0; qi < len(queue); qi++ {
		it := queue[qi]
		if known, ok := generations[it.oid]; ok && known <= it.gen {
			continue
		}
		generations[it.oid] = it.gen
		if maxDepth >= 0 && it.gen+1 >= maxDepth {
			continue
		}
		obj := lookup(it.oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		for _, parent := range ParseCommit(obj.Data).Parents {
			queue = append(queue, item{parent, it.gen + 1})
		}
	}
	return generations
}
