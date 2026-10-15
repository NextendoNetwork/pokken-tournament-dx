package main

// Group Match = NEX communities (PersistentGathering). The core has no community support, so the
// MatchmakeExtension community calls are served here from a store saved to
// POKKEN_COMMUNITY_FILE; leaving/deleting a group arrives through the MatchMaking(Ext)
// EndParticipation/UnregisterGathering calls and is intercepted when the gid is a community.

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"sync"

	nex "github.com/NextendoNetwork/nextendo-nex"
)

const (
	methodCreateCommunity            uint32 = 0x11
	methodUpdateCommunity            uint32 = 0x12
	methodJoinCommunity              uint32 = 0x13
	methodFindCommunityByGatheringID uint32 = 0x14
	methodFindOfficialCommunity      uint32 = 0x15
	methodFindCommunityByParticipant uint32 = 0x16
	methodGetSimpleCommunity         uint32 = 0x20
	methodFindCommunityByOwner       uint32 = 0x36
	// Undocumented; Group Match's group search (PID + 20 filters + range 0/20). The core answers
	// it with SSBU's bare u32 = 2, which reads as a 2-entry list here and hangs the screen.
	methodBrowseCommunity uint32 = 0x3A
	// Undocumented; the owner posts its group summary right after CreateCommunity (same structure
	// the 0x3A search returns: writer main+0x943558, reader main+0x9436e0), answer u32. The core
	// would treat it as SSBU's arena-code create.
	methodPostCommunitySummary uint32 = 0x37

	// MatchMaking (0x15) UnregisterGathering and MatchMakingExt (0x32) EndParticipation.
	methodUnregisterGathering uint32 = 2
	methodEndParticipation    uint32 = 1

	communityGIDBase uint32 = 0x40000000 // apart from the core's matchmake session gids

	resultInvalidPassword uint32 = 0x800300C9 // RendezVous::InvalidGatheringPassword
	resultSessionFull     uint32 = 0x800300C8 // RendezVous::SessionFull
	resultInvalidGID      uint32 = 0x8003006D // RendezVous::InvalidGID
)

// community is a PersistentGathering plus its members.
type community struct {
	ID                  uint32
	OwnerPID            uint64
	HostPID             uint64
	MinParticipants     uint16
	MaxParticipants     uint16
	ParticipationPolicy uint32
	PolicyArgument      uint32
	Flags               uint32
	State               uint32
	Description         string
	CommunityType       uint32
	Password            string
	Attribs             []uint32
	AppBuffer           []byte
	StartDate           uint64
	EndDate             uint64
	SessionCount        uint32
	Members             []uint64
	// Summary is the owner's 0x6D.0x37 group summary, stored verbatim (struct header included)
	// and handed out by the 0x3A search.
	Summary []byte
}

// Levels implements nex.Structure: Gathering, then PersistentGathering.
func (c *community) Levels() []nex.Level {
	return []nex.Level{
		{
			Save: func(o *nex.StreamOut) {
				o.U32(c.ID)
				o.PID(c.OwnerPID)
				o.PID(c.HostPID)
				o.U16(c.MinParticipants)
				o.U16(c.MaxParticipants)
				o.U32(c.ParticipationPolicy)
				o.U32(c.PolicyArgument)
				o.U32(c.Flags)
				o.U32(c.State)
				o.String(c.Description)
			},
			Load: func(i *nex.StreamIn) {
				c.ID = i.U32()
				c.OwnerPID = i.PID()
				c.HostPID = i.PID()
				c.MinParticipants = i.U16()
				c.MaxParticipants = i.U16()
				c.ParticipationPolicy = i.U32()
				c.PolicyArgument = i.U32()
				c.Flags = i.U32()
				c.State = i.U32()
				c.Description = i.String()
			},
		},
		{
			Save: func(o *nex.StreamOut) {
				o.U32(c.CommunityType)
				o.String(c.Password)
				nex.WriteList(o, c.Attribs, func(o *nex.StreamOut, v uint32) { o.U32(v) })
				o.Buffer(c.AppBuffer)
				o.DateTime(c.StartDate)
				o.DateTime(c.EndDate)
				o.U32(c.SessionCount)
				o.U32(uint32(len(c.Members)))
			},
			Load: func(i *nex.StreamIn) {
				c.CommunityType = i.U32()
				c.Password = i.String()
				c.Attribs = nex.ReadList(i, func(i *nex.StreamIn) uint32 { return i.U32() })
				c.AppBuffer = i.Buffer()
				c.StartDate = i.DateTime()
				c.EndDate = i.DateTime()
				c.SessionCount = i.U32()
				i.U32() // participation count: derived from Members
			},
		},
	}
}

var (
	cmMu      sync.Mutex
	cmByID    = map[uint32]*community{}
	cmNextGID = communityGIDBase
	cmFile    = envOr("POKKEN_COMMUNITY_FILE", "pokken_communities.json")
)

func init() {
	b, err := os.ReadFile(cmFile)
	if err != nil {
		return
	}
	var list []*community
	if err := json.Unmarshal(b, &list); err != nil {
		fmt.Printf("[Pokken Community] %s unreadable: %v\n", cmFile, err)
		return
	}
	// A group only exists while its owner is connected, and nobody is connected at startup: keep
	// the gid counter, drop the groups (a stale one traps searchers in a leave/search loop).
	for _, c := range list {
		if c.ID >= cmNextGID {
			cmNextGID = c.ID + 1
		}
	}
	cmSaveLocked()
	fmt.Printf("[Pokken Community] dropped %d stale group(s) from %s\n", len(list), cmFile)
}

// closeCommunitiesOf runs when a player disconnects: their groups close, and they leave the rest.
func closeCommunitiesOf(pid uint64) {
	cmMu.Lock()
	defer cmMu.Unlock()
	changed := false
	for gid, c := range cmByID {
		if c.OwnerPID == pid {
			delete(cmByID, gid)
			changed = true
			fmt.Printf("[Pokken Community] pid=%d disconnected: closed gid=%#x\n", pid, gid)
		} else if slices.Contains(c.Members, pid) {
			c.Members = slices.DeleteFunc(c.Members, func(m uint64) bool { return m == pid })
			changed = true
		}
	}
	if changed {
		cmSaveLocked()
	}
}

// cmSaveLocked writes every group to disk; cmMu must be held.
func cmSaveLocked() {
	b, err := json.Marshal(cmSortedLocked(func(*community) bool { return true }))
	if err == nil {
		err = os.WriteFile(cmFile+".tmp", b, 0o644)
	}
	if err == nil {
		err = os.Rename(cmFile+".tmp", cmFile)
	}
	if err != nil {
		fmt.Printf("[Pokken Community] save failed: %v\n", err)
	}
}

func cmSortedLocked(keep func(*community) bool) []*community {
	var out []*community
	for _, c := range cmByID {
		if keep(c) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// readRange consumes a ResultRange structure.
func readRange(in *nex.StreamIn) (offset, size uint32) {
	r := readStructHeader(in)
	return r.U32(), r.U32()
}

func pageOf(list []*community, offset, size uint32) []*community {
	if int(offset) >= len(list) {
		return nil
	}
	list = list[offset:]
	if size != 0 && int(size) < len(list) {
		list = list[:size]
	}
	return list
}

func communityList(conn *nex.Connection, req *nex.RMCMessage, list []*community, what string) *nex.RMCMessage {
	out := nex.NewStreamOut(conn.Settings)
	nex.WriteList(out, list, func(o *nex.StreamOut, c *community) { o.Add(c) })
	fmt.Printf("[Pokken Community] pid=%d %s -> %d group(s)\n", conn.PID, what, len(list))
	return nex.NewRMCSuccess(conn.Settings, nex.ProtocolMatchmakeExtension, req.Method, req.CallID, out.Bytes())
}

func mmeOK(conn *nex.Connection, req *nex.RMCMessage, body []byte) *nex.RMCMessage {
	return nex.NewRMCSuccess(conn.Settings, nex.ProtocolMatchmakeExtension, req.Method, req.CallID, body)
}

func mmeErr(conn *nex.Connection, req *nex.RMCMessage, code uint32) *nex.RMCMessage {
	return nex.NewRMCError(conn.Settings, nex.ProtocolMatchmakeExtension, req.CallID, code)
}

// withCommunities wraps the core MatchmakeExtension handler.
func withCommunities(next nex.RMCHandler) nex.RMCHandler {
	return func(conn *nex.Connection, req *nex.RMCMessage) *nex.RMCMessage {
		s := conn.Settings
		in := nex.NewStreamIn(req.Body, s)
		switch req.Method {
		case methodCreateCommunity:
			c := &community{}
			in.Extract(c)
			msg := in.String()
			if in.Err() != nil {
				fmt.Printf("[Pokken Community] pid=%d CreateCommunity parse error %v body=%x\n", conn.PID, in.Err(), req.Body)
				return mmeErr(conn, req, nex.ResultCoreInvalidArgument)
			}
			cmMu.Lock()
			c.ID = cmNextGID
			cmNextGID++
			c.OwnerPID, c.HostPID = conn.PID, conn.PID
			c.Members = []uint64{conn.PID}
			c.StartDate = uint64(nex.NowDateTime())
			cmByID[c.ID] = c
			cmSaveLocked()
			cmMu.Unlock()
			out := nex.NewStreamOut(s)
			out.U32(c.ID)
			fmt.Printf("[Pokken Community] pid=%d CreateCommunity gid=%#x desc=%q type=%d max=%d pw=%v attribs=%v msg=%q\n",
				conn.PID, c.ID, c.Description, c.CommunityType, c.MaxParticipants, c.Password != "", c.Attribs, msg)
			return mmeOK(conn, req, out.Bytes())

		case methodUpdateCommunity:
			u := &community{}
			in.Extract(u)
			if in.Err() != nil {
				return mmeErr(conn, req, nex.ResultCoreInvalidArgument)
			}
			cmMu.Lock()
			c := cmByID[u.ID]
			if c == nil || c.OwnerPID != conn.PID {
				cmMu.Unlock()
				return mmeErr(conn, req, resultInvalidGID)
			}
			c.MinParticipants, c.MaxParticipants = u.MinParticipants, u.MaxParticipants
			c.ParticipationPolicy, c.PolicyArgument, c.Flags, c.State = u.ParticipationPolicy, u.PolicyArgument, u.Flags, u.State
			c.Description, c.Password, c.Attribs, c.AppBuffer = u.Description, u.Password, u.Attribs, u.AppBuffer
			cmSaveLocked()
			cmMu.Unlock()
			fmt.Printf("[Pokken Community] pid=%d UpdateCommunity gid=%#x desc=%q\n", conn.PID, u.ID, u.Description)
			return mmeOK(conn, req, nil)

		case methodJoinCommunity:
			gid := in.U32()
			_ = in.String() // message
			pw := in.String()
			cmMu.Lock()
			defer cmMu.Unlock()
			c := cmByID[gid]
			switch {
			case c == nil:
				return mmeErr(conn, req, resultInvalidGID)
			case c.Password != "" && c.Password != pw:
				return mmeErr(conn, req, resultInvalidPassword)
			case slices.Contains(c.Members, conn.PID):
			case c.MaxParticipants != 0 && len(c.Members) >= int(c.MaxParticipants):
				return mmeErr(conn, req, resultSessionFull)
			default:
				c.Members = append(c.Members, conn.PID)
				cmSaveLocked()
			}
			fmt.Printf("[Pokken Community] pid=%d JoinCommunity gid=%#x -> %d member(s)\n", conn.PID, gid, len(c.Members))
			return mmeOK(conn, req, nil)

		case methodFindCommunityByGatheringID:
			ids := nex.ReadList(in, func(i *nex.StreamIn) uint32 { return i.U32() })
			cmMu.Lock()
			var list []*community
			for _, id := range ids {
				if c := cmByID[id]; c != nil {
					list = append(list, c)
				}
			}
			resp := communityList(conn, req, list, fmt.Sprintf("FindCommunityByGatheringId %#x", ids))
			cmMu.Unlock()
			return resp

		case methodFindOfficialCommunity:
			return communityList(conn, req, nil, "FindOfficialCommunity")

		case methodFindCommunityByParticipant, methodFindCommunityByOwner:
			pid := in.PID()
			off, size := readRange(in)
			byOwner := req.Method == methodFindCommunityByOwner
			cmMu.Lock()
			list := cmSortedLocked(func(c *community) bool {
				if byOwner {
					return c.OwnerPID == pid
				}
				return c.OwnerPID != pid && slices.Contains(c.Members, pid)
			})
			name := "FindCommunityByParticipant"
			if byOwner {
				name = "FindCommunityByOwner"
			}
			resp := communityList(conn, req, pageOf(list, off, size), fmt.Sprintf("%s(%d)", name, pid))
			cmMu.Unlock()
			return resp

		case methodBrowseCommunity:
			// Open groups the caller is not in, newest first.
			cmMu.Lock()
			list := cmSortedLocked(func(c *community) bool {
				return !slices.Contains(c.Members, conn.PID) &&
					(c.MaxParticipants == 0 || len(c.Members) < int(c.MaxParticipants))
			})
			slices.Reverse(list)
			if len(list) > 20 {
				list = list[:20]
			}
			out := nex.NewStreamOut(s)
			nex.WriteList(out, list, func(o *nex.StreamOut, c *community) {
				if len(c.Summary) > 0 {
					o.Write(c.Summary)
					return
				}
				writeSearchResult(o, c)
			})
			cmMu.Unlock()
			fmt.Printf("[Pokken Community] pid=%d BrowseCommunity -> %d group(s)\n", conn.PID, len(list))
			return mmeOK(conn, req, out.Bytes())

		case methodPostCommunitySummary:
			// Body: one framed summary {u32 id, u64, List<u32>, qBuffer, String, {...}, u32, DateTime}.
			sum := append([]byte(nil), req.Body...)
			cmMu.Lock()
			var c *community
			for _, g := range cmByID {
				if g.OwnerPID == conn.PID && (c == nil || g.ID > c.ID) {
					c = g
				}
			}
			var gid uint32
			if c != nil {
				gid = c.ID
				// The first field (after the 5-byte header) is the group id the joiner looks up.
				if len(sum) >= 9 && sum[5] == 0 && sum[6] == 0 && sum[7] == 0 && sum[8] == 0 {
					sum[5], sum[6], sum[7], sum[8] = byte(gid), byte(gid>>8), byte(gid>>16), byte(gid>>24)
				}
				c.Summary = sum
				cmSaveLocked()
			}
			cmMu.Unlock()
			var first uint32
			if len(req.Body) >= 9 {
				first = uint32(req.Body[5]) | uint32(req.Body[6])<<8 | uint32(req.Body[7])<<16 | uint32(req.Body[8])<<24
			}
			fmt.Printf("[Pokken Community] pid=%d 0x37 summary len=%d firstU32=%#x -> stored on gid=%#x\n", conn.PID, len(req.Body), first, gid)
			out := nex.NewStreamOut(s)
			out.U32(gid)
			return mmeOK(conn, req, out.Bytes())

		case methodGetSimpleCommunity:
			ids := nex.ReadList(in, func(i *nex.StreamIn) uint32 { return i.U32() })
			cmMu.Lock()
			out := nex.NewStreamOut(s)
			var found []*community
			for _, id := range ids {
				if c := cmByID[id]; c != nil {
					found = append(found, c)
				}
			}
			nex.WriteList(out, found, func(o *nex.StreamOut, c *community) {
				writeStruct(o, func(o *nex.StreamOut) {
					o.U32(c.ID)
					o.U32(c.SessionCount)
				})
			})
			cmMu.Unlock()
			fmt.Printf("[Pokken Community] pid=%d GetSimpleCommunity %#x -> %d\n", conn.PID, ids, len(found))
			return mmeOK(conn, req, out.Bytes())
		}
		return next(conn, req)
	}
}

// writeSearchResult writes one 0x3A search result. Not a PersistentGathering: the client's reader
// (list main+0x946158, element main+0x9436e0, nested main+0x942eec) takes
// {u32, u64, List<u32>, qBuffer (u16 len, main+0x655dc8), String, {u32 x4, DateTime x2}, u32,
// DateTime}, each struct framed.
func writeSearchResult(o *nex.StreamOut, c *community) {
	writeStruct(o, func(o *nex.StreamOut) {
		o.U32(c.ID)
		o.PID(c.OwnerPID)
		// The search carries 20 attribute filters; 6 attributes here throws a C++ exception in
		// the client (out-of-range index), so pad to 20.
		attribs := make([]uint32, max(20, len(c.Attribs)))
		copy(attribs, c.Attribs)
		nex.WriteList(o, attribs, func(o *nex.StreamOut, v uint32) { o.U32(v) })
		o.QBuffer(c.AppBuffer)
		o.String(c.Description)
		writeStruct(o, func(o *nex.StreamOut) {
			o.U32(uint32(len(c.Members)))
			o.U32(uint32(c.MaxParticipants))
			o.U32(c.SessionCount)
			o.U32(c.ParticipationPolicy)
			o.DateTime(c.StartDate)
			o.DateTime(c.EndDate)
		})
		o.U32(c.CommunityType)
		o.DateTime(c.StartDate)
	})
}

// leaveCommunity handles a gid that belongs to a group: the owner's UnregisterGathering closes it
// (sent when the owner leaves it), a member's EndParticipation leaves it. Returns false when gid is
// not a group (the core handles it).
func leaveCommunity(pid uint64, gid uint32, unregister bool) bool {
	cmMu.Lock()
	defer cmMu.Unlock()
	c := cmByID[gid]
	if c == nil {
		return false
	}
	if unregister && c.OwnerPID == pid {
		delete(cmByID, gid)
		fmt.Printf("[Pokken Community] pid=%d closed gid=%#x\n", pid, gid)
	} else {
		c.Members = slices.DeleteFunc(c.Members, func(m uint64) bool { return m == pid })
		fmt.Printf("[Pokken Community] pid=%d left gid=%#x -> %d member(s)\n", pid, gid, len(c.Members))
	}
	cmSaveLocked()
	return true
}

// withCommunityLeave wraps MatchMaking (UnregisterGathering) or MatchMakingExt (EndParticipation).
func withCommunityLeave(protocol uint16, method uint32, next nex.RMCHandler) nex.RMCHandler {
	return func(conn *nex.Connection, req *nex.RMCMessage) *nex.RMCMessage {
		if req.Method == method {
			in := nex.NewStreamIn(req.Body, conn.Settings)
			gid := in.U32()
			if in.Err() == nil && gid >= communityGIDBase &&
				leaveCommunity(conn.PID, gid, method == methodUnregisterGathering) {
				out := nex.NewStreamOut(conn.Settings)
				out.Bool(true)
				return nex.NewRMCSuccess(conn.Settings, protocol, req.Method, req.CallID, out.Bytes())
			}
		}
		return next(conn, req)
	}
}
