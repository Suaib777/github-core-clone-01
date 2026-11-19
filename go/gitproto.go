// gitproto.go — git smart HTTP: info/refs (v0 + v2), upload-pack v2 (ls-refs,
// fetch) and receive-pack v0 (push with report-status). Ported from the
// TypeScript forge (gitproto.ts), keeping the wire behaviour identical.

package main

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const Agent = "forge/1.0"

func pktText(s string) []byte { return PktLine([]byte(s)) }

// sidebandPack splits pack bytes into sideband (band 1) pkt-lines of at most
// ~32 KiB of payload.
func sidebandPack(pack []byte) []byte {
	var buf bytes.Buffer
	const chunk = 32768
	for off := 0; off < len(pack); off += chunk {
		end := off + chunk
		if end > len(pack) {
			end = len(pack)
		}
		payload := make([]byte, 0, 1+(end-off))
		payload = append(payload, 1)
		payload = append(payload, pack[off:end]...)
		buf.Write(PktLine(payload))
	}
	if len(pack) == 0 {
		buf.Write(PktLine([]byte{1}))
	}
	return buf.Bytes()
}

type refPair struct {
	name string
	oid  string
}

// repoRefs returns all refs of a repo as (name, oid) pairs, HEAD first, then
// branches, then tags, sorted.
func repoRefs(repo *Repo) []refPair {
	var out []refPair
	if tip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
		out = append(out, refPair{"HEAD", tip})
	}
	var branches []string
	for ref := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			branches = append(branches, ref)
		}
	}
	sort.Strings(branches)
	for _, ref := range branches {
		out = append(out, refPair{ref, repo.Refs[ref]})
	}
	var tags []string
	for ref := range repo.Refs {
		if strings.HasPrefix(ref, "refs/tags/") {
			tags = append(tags, ref)
		}
	}
	sort.Strings(tags)
	for _, ref := range tags {
		out = append(out, refPair{ref, repo.Refs[ref]})
	}
	return out
}

// peeledOID returns the peeled commit of a tag ref, or "".
func peeledOID(repo *Repo, oid string) string {
	obj := repo.Objects[oid]
	if obj == nil || obj.Type != ObjTag {
		return ""
	}
	return PeelToCommit(oid, func(id string) *GitObject { return repo.Objects[id] })
}

// ---------------------------------------------------------------------------
// info/refs
// ---------------------------------------------------------------------------

func v2Advertisement(service string) []byte {
	lines := []string{
		"# service=" + service + "\n",
		"version 2\n",
		"agent=" + Agent + "\n",
		"object-format=sha1\n",
		"ls-refs=unborn\n",
		"fetch=shallow filter\n",
		"server-option\n",
		"object-info\n",
	}
	controls := []string{"", "flush", "", "", "", "", "", "", "", "flush"}
	// First element is the "# service" line; the flush follows it.
	raw := make([][]byte, len(lines))
	for i, s := range lines {
		raw[i] = []byte(s)
	}
	_ = raw
	// Build explicitly to keep the flush positions exact:
	// "# service=...\n", flush, "version 2\n", caps..., flush.
	var buf bytes.Buffer
	buf.Write(PktLine([]byte("# service=" + service + "\n")))
	buf.Write(pktFlush)
	buf.Write(PktLine([]byte("version 2\n")))
	buf.Write(PktLine([]byte("agent=" + Agent + "\n")))
	buf.Write(PktLine([]byte("object-format=sha1\n")))
	buf.Write(PktLine([]byte("ls-refs=unborn\n")))
	buf.Write(PktLine([]byte("fetch=shallow filter\n")))
	buf.Write(PktLine([]byte("server-option\n")))
	buf.Write(PktLine([]byte("object-info\n")))
	buf.Write(pktFlush)
	_ = controls
	return buf.Bytes()
}

func v0Advertisement(repo *Repo, service string) []byte {
	var caps string
	if service == "git-upload-pack" {
		caps = fmt.Sprintf("symref=HEAD:refs/heads/%s agent=%s side-band-64k thin-pack ofs-delta multi_ack shallow", repo.DefaultBranch, Agent)
	} else {
		caps = fmt.Sprintf("report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=%s", Agent)
	}
	var buf bytes.Buffer
	buf.Write(pktText("# service=" + service + "\n"))
	buf.Write(pktFlush)
	refs := repoRefs(repo)
	if len(refs) == 0 {
		buf.Write(pktText(fmt.Sprintf("%s capabilities^{}\x00%s\n", ZeroOID, caps)))
	} else {
		first := true
		for _, ref := range refs {
			var line string
			if first {
				line = fmt.Sprintf("%s %s\x00%s\n", ref.oid, ref.name, caps)
			} else {
				line = fmt.Sprintf("%s %s\n", ref.oid, ref.name)
			}
			buf.Write(pktText(line))
			first = false
			if peeled := peeledOID(repo, ref.oid); peeled != "" {
				buf.Write(pktText(fmt.Sprintf("%s %s^{}\n", peeled, ref.name)))
			}
		}
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

type gitResult struct {
	Status      int
	ContentType string
	Body        []byte
}

func jsonErrResult(status int) gitResult {
	return gitResult{Status: status, ContentType: "application/json; charset=utf-8", Body: []byte(`{"message":"Not Found"}` + "\n")}
}

// InfoRefs handles GET /{owner}/{repo}.git/info/refs.
func InfoRefs(store *Store, owner, repoName, service string, gitProtocol string) gitResult {
	repo := store.GetRepo(owner, repoName)
	if repo == nil {
		return jsonErrResult(404)
	}
	if service == "git-upload-pack" {
		if gitProtocol == "version=2" {
			return gitResult{Status: 200, ContentType: "application/x-git-upload-pack-advertisement", Body: v2Advertisement("git-upload-pack")}
		}
		return gitResult{Status: 200, ContentType: "application/x-git-upload-pack-advertisement", Body: v0Advertisement(repo, "git-upload-pack")}
	}
	if service == "git-receive-pack" {
		return gitResult{Status: 200, ContentType: "application/x-git-receive-pack-advertisement", Body: v0Advertisement(repo, "git-receive-pack")}
	}
	return jsonErrResult(404)
}

// ---------------------------------------------------------------------------
// upload-pack v2: ls-refs
// ---------------------------------------------------------------------------

type lsRefsArgs struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

func parseLsRefsArgs(packets []Pkt) lsRefsArgs {
	var args lsRefsArgs
	for _, packet := range packets {
		if packet.Kind != PktData {
			continue
		}
		line := strings.TrimSuffix(string(packet.Payload), "\n")
		switch {
		case line == "peel":
			args.peel = true
		case line == "symrefs":
			args.symrefs = true
		case line == "unborn":
			args.unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			args.prefixes = append(args.prefixes, line[len("ref-prefix "):])
		}
	}
	return args
}

func lsRefsResponse(repo *Repo, args lsRefsArgs) []byte {
	var buf bytes.Buffer
	tip, hasTip := repo.Refs["refs/heads/"+repo.DefaultBranch]
	matches := func(name string) bool {
		if len(args.prefixes) == 0 {
			return true
		}
		for _, p := range args.prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	if len(args.prefixes) == 0 {
		if hasTip {
			symref := ""
			if args.symrefs {
				symref = " symref-target:refs/heads/" + repo.DefaultBranch
			}
			buf.Write(pktText(fmt.Sprintf("%s HEAD%s\n", tip, symref)))
		} else if args.unborn {
			symref := ""
			if args.symrefs {
				symref = " symref-target:refs/heads/" + repo.DefaultBranch
			}
			buf.Write(pktText(fmt.Sprintf("unborn HEAD%s\n", symref)))
		}
	}
	refs := make([]string, 0, len(repo.Refs))
	for ref := range repo.Refs {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		if !matches(ref) {
			continue
		}
		oid := repo.Refs[ref]
		line := oid + " " + ref
		if args.peel {
			if peeled := peeledOID(repo, oid); peeled != "" {
				line += " peeled:" + peeled
			}
		}
		buf.Write(pktText(line + "\n"))
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// upload-pack v2: fetch
// ---------------------------------------------------------------------------

type fetchArgs struct {
	wants       []string
	haves       []string
	shallow     []string
	deepen      int
	hasDeepen   bool
	deepenSince int64
	hasDeepenSince bool
	deepenNot   []string
}

func parseFetchArgs(packets []Pkt) fetchArgs {
	var args fetchArgs
	for _, packet := range packets {
		if packet.Kind != PktData {
			continue
		}
		line := strings.TrimSuffix(string(packet.Payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			args.wants = append(args.wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			args.haves = append(args.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			args.shallow = append(args.shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				args.deepen = n
				args.hasDeepen = true
			}
		case strings.HasPrefix(line, "deepen-since "):
			if n, err := strconv.ParseInt(strings.TrimSpace(line[12:]), 10, 64); err == nil {
				args.deepenSince = n
				args.hasDeepenSince = true
			}
		case strings.HasPrefix(line, "deepen-not "):
			args.deepenNot = append(args.deepenNot, line[11:])
		}
	}
	return args
}

type fetchResult struct {
	objects   []*GitObject
	shallow   []string
	unshallow []string
	acks      []string
	nak       bool
}

func computeFetch(repo *Repo, args fetchArgs) fetchResult {
	lookup := func(oid string) *GitObject { return repo.Objects[oid] }
	shallowBoundaries := make(map[string]bool)
	for _, s := range args.shallow {
		shallowBoundaries[s] = true
	}
	// A want that names a tag is really a want for the commit it peels to.
	wantCommits := make([]string, 0, len(args.wants))
	for _, oid := range args.wants {
		obj := lookup(oid)
		if obj != nil && obj.Type == ObjTag {
			wantCommits = append(wantCommits, PeelToCommit(oid, lookup))
		} else {
			wantCommits = append(wantCommits, oid)
		}
	}
	// What the client already holds.
	haveSet := make(map[string]bool)
	{
		stack := append([]string(nil), args.haves...)
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if haveSet[oid] {
				continue
			}
			obj := lookup(oid)
			if obj == nil {
				continue
			}
			haveSet[oid] = true
			switch obj.Type {
			case ObjCommit:
				info := ParseCommit(obj.Data)
				stack = append(stack, info.Tree)
				if !shallowBoundaries[oid] {
					stack = append(stack, info.Parents...)
				}
			case ObjTree:
				entries, err := ParseTree(obj.Data)
				if err == nil {
					for _, e := range entries {
						stack = append(stack, e.OID)
					}
				}
			case ObjTag:
				stack = append(stack, ParseTag(obj.Data).Object)
			}
		}
	}
	sendCommits := make(map[string]bool)
	newShallow := make(map[string]bool)
	unshallow := make(map[string]bool)
	if args.hasDeepen && args.deepen > 0 {
		generations := CommitGenerations(wantCommits, lookup, args.deepen)
		for oid, gen := range generations {
			sendCommits[oid] = true
			if gen == args.deepen-1 {
				if obj := lookup(oid); obj != nil && obj.Type == ObjCommit && len(ParseCommit(obj.Data).Parents) > 0 {
					newShallow[oid] = true
				}
			}
		}
		for boundary := range shallowBoundaries {
			if !sendCommits[boundary] {
				continue
			}
			obj := lookup(boundary)
			if obj == nil || obj.Type != ObjCommit {
				continue
			}
			for _, parent := range ParseCommit(obj.Data).Parents {
				if sendCommits[parent] {
					unshallow[boundary] = true
					break
				}
			}
		}
	} else if args.hasDeepenSince {
		generations := CommitGenerations(wantCommits, lookup, -1)
		for oid := range generations {
			obj := lookup(oid)
			if obj == nil || obj.Type != ObjCommit {
				continue
			}
			info := ParseCommit(obj.Data)
			ts := commitTimestamp(info.Author)
			if ts != -1 && ts >= args.deepenSince {
				sendCommits[oid] = true
			} else {
				newShallow[oid] = true
			}
		}
	} else {
		for oid := range CollectReachable(args.wants, lookup) {
			if obj := lookup(oid); obj != nil && obj.Type == ObjCommit {
				sendCommits[oid] = true
			}
		}
	}
	// The objects to send.
	toSend := make(map[string]bool)
	{
		stack := make([]string, 0, len(sendCommits)+len(args.wants))
		for oid := range sendCommits {
			stack = append(stack, oid)
		}
		stack = append(stack, args.wants...)
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if toSend[oid] || haveSet[oid] {
				continue
			}
			obj := lookup(oid)
			if obj == nil {
				continue
			}
			toSend[oid] = true
			switch obj.Type {
			case ObjCommit:
				info := ParseCommit(obj.Data)
				stack = append(stack, info.Tree)
				if !newShallow[oid] {
					stack = append(stack, info.Parents...)
				}
			case ObjTree:
				entries, err := ParseTree(obj.Data)
				if err == nil {
					for _, e := range entries {
						stack = append(stack, e.OID)
					}
				}
			case ObjTag:
				stack = append(stack, ParseTag(obj.Data).Object)
			}
		}
	}
	// Deterministic pack order: sort the oids (the TS forge emits them in
	// set-insertion order; the client parses the pack either way, and the
	// state export holds only pass/fail bits).
	oids := make([]string, 0, len(toSend))
	for oid := range toSend {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	var objects []*GitObject
	for _, oid := range oids {
		if obj := lookup(oid); obj != nil {
			objects = append(objects, obj)
		}
	}
	var acks []string
	nak := false
	if len(args.haves) == 0 {
		nak = true
	} else {
		for _, have := range args.haves {
			if lookup(have) != nil {
				acks = append(acks, have)
			}
		}
		if len(acks) == 0 {
			nak = true
		}
	}
	shallowList := make([]string, 0, len(newShallow))
	for oid := range newShallow {
		shallowList = append(shallowList, oid)
	}
	sort.Strings(shallowList)
	unshallowList := make([]string, 0, len(unshallow))
	for oid := range unshallow {
		unshallowList = append(unshallowList, oid)
	}
	sort.Strings(unshallowList)
	return fetchResult{objects: objects, shallow: shallowList, unshallow: unshallowList, acks: acks, nak: nak}
}

func commitTimestamp(identity string) int64 {
	// / (\d+) [+-]\d{4}$/
	i := strings.LastIndex(identity, " ")
	if i < 0 {
		return -1
	}
	tz := identity[i+1:]
	if len(tz) != 5 || (tz[0] != '+' && tz[0] != '-') {
		return -1
	}
	for _, c := range tz[1:] {
		if c < '0' || c > '9' {
			return -1
		}
	}
	rest := identity[:i]
	j := strings.LastIndex(rest, " ")
	if j < 0 {
		return -1
	}
	n, err := strconv.ParseInt(rest[j+1:], 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func fetchV2Response(repo *Repo, args fetchArgs) []byte {
	result := computeFetch(repo, args)
	var buf bytes.Buffer
	buf.Write(pktText("acknowledgments\n"))
	if result.nak {
		buf.Write(pktText("NAK\n"))
	}
	for _, ack := range result.acks {
		buf.Write(pktText("ACK " + ack + "\n"))
	}
	if !result.nak {
		buf.Write(pktText("ready\n"))
	}
	buf.Write(pktDelim)
	if len(result.shallow) > 0 || len(result.unshallow) > 0 {
		buf.Write(pktText("shallow-info\n"))
		for _, oid := range result.shallow {
			buf.Write(pktText("shallow " + oid + "\n"))
		}
		for _, oid := range result.unshallow {
			buf.Write(pktText("unshallow " + oid + "\n"))
		}
		buf.Write(pktDelim)
	}
	buf.Write(pktText("packfile\n"))
	objs := make([]GitObject, len(result.objects))
	for i, o := range result.objects {
		objs[i] = *o
	}
	buf.Write(sidebandPack(WritePack(objs)))
	buf.Write(pktFlush)
	return buf.Bytes()
}

func fetchV0Response(repo *Repo, body []byte) []byte {
	packets, _ := DecodePktLines(body)
	var args fetchArgs
	seenFlush := false
	for _, packet := range packets {
		if packet.Kind == PktFlush {
			seenFlush = true
			continue
		}
		if packet.Kind != PktData {
			continue
		}
		line := strings.TrimSuffix(string(packet.Payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			args.wants = append(args.wants, strings.Fields(line[5:])[0])
		case strings.HasPrefix(line, "shallow "):
			args.shallow = append(args.shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				args.deepen = n
				args.hasDeepen = true
			}
		case seenFlush && strings.HasPrefix(line, "have "):
			args.haves = append(args.haves, strings.TrimSpace(line[5:]))
		}
	}
	result := computeFetch(repo, args)
	var buf bytes.Buffer
	if result.nak {
		buf.Write(pktText("NAK\n"))
	}
	for _, ack := range result.acks {
		buf.Write(pktText("ACK " + ack + "\n"))
	}
	for _, oid := range result.shallow {
		buf.Write(pktText("shallow " + oid + "\n"))
	}
	for _, oid := range result.unshallow {
		buf.Write(pktText("unshallow " + oid + "\n"))
	}
	objs := make([]GitObject, len(result.objects))
	for i, o := range result.objects {
		objs[i] = *o
	}
	buf.Write(sidebandPack(WritePack(objs)))
	buf.Write(pktFlush)
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// receive-pack v0: push
// ---------------------------------------------------------------------------

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

// parsePushBody parses a receive-pack body: command lines, then the pack.
func parsePushBody(body []byte) ([]pushCommand, []byte, bool) {
	var commands []pushCommand
	off := 0
	flushEnd := -1
	for off < len(body) {
		if off+4 > len(body) {
			return nil, nil, false
		}
		head := string(body[off : off+4])
		if head == "0000" {
			flushEnd = off + 4
			break
		}
		if head == "0001" {
			off += 4
			continue
		}
		length, err := strconv.ParseInt(head, 16, 32)
		if err != nil || length < 4 || off+int(length) > len(body) {
			return nil, nil, false
		}
		payload := body[off+4 : off+int(length)]
		text := string(payload)
		if nul := strings.IndexByte(text, 0); nul >= 0 {
			text = text[:nul]
		}
		line := strings.TrimSuffix(text, "\n")
		parts := strings.Split(line, " ")
		if len(parts) < 3 {
			return nil, nil, false
		}
		oldOID, newOID, ref := parts[0], parts[1], parts[2]
		if !isHex40(oldOID) || !isHex40(newOID) {
			return nil, nil, false
		}
		commands = append(commands, pushCommand{oldOID, newOID, ref})
		off += int(length)
	}
	if len(commands) == 0 || flushEnd < 0 {
		return nil, nil, false
	}
	return commands, body[flushEnd:], true
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// ReceivePack handles POST git-receive-pack.
func ReceivePack(store *Store, owner, repoName string, body []byte) gitResult {
	repo := store.GetRepo(owner, repoName)
	if repo == nil {
		return jsonErrResult(404)
	}
	commands, pack, ok := parsePushBody(body)
	if !ok {
		report := EncodePktLines([][]byte{append([]byte{1}, []byte("unpack error\n")...)}, []string{""})
		return gitResult{Status: 200, ContentType: "application/x-git-receive-pack-result", Body: append(report, pktFlush...)}
	}
	lookup := func(oid string) *GitObject { return repo.Objects[oid] }
	unpackOK := true
	if len(pack) > 0 {
		read, err := ReadPack(pack, lookup)
		if err != nil {
			unpackOK = false
		} else {
			for oid, obj := range read.Objects {
				if _, exists := repo.Objects[oid]; !exists {
					repo.Objects[oid] = obj
				}
			}
		}
	}
	var reportLines []string
	if !unpackOK {
		reportLines = append(reportLines, "unpack error\n")
	} else {
		reportLines = append(reportLines, "unpack ok\n")
	}
	if unpackOK {
		for _, cmd := range commands {
			current, hasCurrent := repo.Refs[cmd.ref]
			if cmd.newOID == ZeroOID {
				// Delete.
				switch {
				case !hasCurrent:
					reportLines = append(reportLines, fmt.Sprintf("ng %s no such ref\n", cmd.ref))
				case cmd.oldOID != ZeroOID && !strings.EqualFold(cmd.oldOID, current):
					reportLines = append(reportLines, fmt.Sprintf("ng %s old value mismatch\n", cmd.ref))
				default:
					delete(repo.Refs, cmd.ref)
					reportLines = append(reportLines, fmt.Sprintf("ok %s\n", cmd.ref))
				}
				continue
			}
			if _, ok := repo.Objects[cmd.newOID]; !ok {
				reportLines = append(reportLines, fmt.Sprintf("ng %s object not found\n", cmd.ref))
				continue
			}
			if !hasCurrent {
				if cmd.oldOID != ZeroOID {
					reportLines = append(reportLines, fmt.Sprintf("ng %s old value mismatch\n", cmd.ref))
					continue
				}
				repo.Refs[cmd.ref] = cmd.newOID
				reportLines = append(reportLines, fmt.Sprintf("ok %s\n", cmd.ref))
				continue
			}
			if !strings.EqualFold(cmd.oldOID, current) {
				reportLines = append(reportLines, fmt.Sprintf("ng %s old value mismatch\n", cmd.ref))
				continue
			}
			newCommit := PeelToCommit(cmd.newOID, lookup)
			oldCommit := PeelToCommit(current, lookup)
			if !IsAncestor(oldCommit, newCommit, lookup) {
				reportLines = append(reportLines, fmt.Sprintf("ng %s non-fast-forward\n", cmd.ref))
				continue
			}
			repo.Refs[cmd.ref] = cmd.newOID
			reportLines = append(reportLines, fmt.Sprintf("ok %s\n", cmd.ref))
		}
	}
	reportText := strings.Join(reportLines, "")
	payload := append([]byte{1}, []byte(reportText)...)
	reportPackets := EncodePktLines([][]byte{payload}, []string{""})
	out := append(reportPackets, pktFlush...)
	return gitResult{Status: 200, ContentType: "application/x-git-receive-pack-result", Body: out}
}

// UploadPack handles POST git-upload-pack (v2 ls-refs/fetch, or v0 fetch).
func UploadPack(store *Store, owner, repoName string, body []byte, gitProtocol string) gitResult {
	repo := store.GetRepo(owner, repoName)
	if repo == nil {
		return jsonErrResult(404)
	}
	packets, err := DecodePktLines(body)
	if err != nil {
		return gitResult{Status: 400, ContentType: "application/json; charset=utf-8", Body: []byte(`{"message":"malformed pkt-line"}` + "\n")}
	}
	if gitProtocol == "version=2" {
		command := ""
		for _, packet := range packets {
			if packet.Kind == PktData {
				command = strings.TrimSuffix(string(packet.Payload), "\n")
				break
			}
		}
		switch command {
		case "command=ls-refs":
			return gitResult{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: lsRefsResponse(repo, parseLsRefsArgs(packets))}
		case "command=fetch":
			return gitResult{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: fetchV2Response(repo, parseFetchArgs(packets))}
		default:
			return gitResult{Status: 400, ContentType: "application/json; charset=utf-8", Body: []byte(`{"message":"unknown command"}` + "\n")}
		}
	}
	return gitResult{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: fetchV0Response(repo, body)}
}
