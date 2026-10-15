package main

// Ranking (0x70). The core's handler is built around Mario Kart's tournament calls, so Pokken's
// rank points are served here: UploadScore at the end of a Rank Match, GetRanking from the Rank
// Points and Ranking screens, common data for the rows. Saved to POKKEN_RANKING_FILE.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"

	nex "github.com/NextendoNetwork/nextendo-nex"
)

const (
	protocolRanking         uint16 = 0x70
	methodUploadScore       uint32 = 1
	methodUploadCommonData  uint32 = 4
	methodGetCommonData     uint32 = 6
	methodGetRanking        uint32 = 9
	rankingModeAroundSelf   uint8  = 1
	rankingModeSelf         uint8  = 4
	resultRankingNotFound   uint32 = 0x80670005 // Ranking::NotFound
	rankingGroupIndexNoFilt uint8  = 0xFF
)

type scoreEntry struct {
	PID       uint64
	UniqueID  uint64
	Category  uint32
	Score     uint32
	OrderBy   uint8
	Groups    []byte
	Param     uint64
	UpdatedAt uint64
}

type rankingStore struct {
	Scores     map[string]*scoreEntry // "category:pid"
	CommonData map[uint64][]byte
}

var (
	rkMu   sync.Mutex
	rk     = rankingStore{Scores: map[string]*scoreEntry{}, CommonData: map[uint64][]byte{}}
	rkFile = envOr("POKKEN_RANKING_FILE", "pokken_ranking.json")
)

func init() {
	b, err := os.ReadFile(rkFile)
	if err != nil {
		return
	}
	var st rankingStore
	if err := json.Unmarshal(b, &st); err != nil {
		fmt.Printf("[Pokken Ranking] %s unreadable: %v\n", rkFile, err)
		return
	}
	if st.Scores != nil {
		rk.Scores = st.Scores
	}
	if st.CommonData != nil {
		rk.CommonData = st.CommonData
	}
	fmt.Printf("[Pokken Ranking] loaded %d score(s) from %s\n", len(rk.Scores), rkFile)
}

// rkSaveLocked writes the store to disk; rkMu must be held.
func rkSaveLocked() {
	b, err := json.Marshal(rk)
	if err == nil {
		err = os.WriteFile(rkFile+".tmp", b, 0o644)
	}
	if err == nil {
		err = os.Rename(rkFile+".tmp", rkFile)
	}
	if err != nil {
		fmt.Printf("[Pokken Ranking] save failed: %v\n", err)
	}
}

func rankingOK(conn *nex.Connection, req *nex.RMCMessage, body []byte) *nex.RMCMessage {
	return nex.NewRMCSuccess(conn.Settings, protocolRanking, req.Method, req.CallID, body)
}

// better reports whether a beats b for orderBy (0 = lower is better, otherwise higher).
func better(a, b uint32, orderBy uint8) bool {
	if orderBy == 0 {
		return a < b
	}
	return a > b
}

func withRanking(next nex.RMCHandler) nex.RMCHandler {
	return func(conn *nex.Connection, req *nex.RMCMessage) *nex.RMCMessage {
		s := conn.Settings
		in := nex.NewStreamIn(req.Body, s)
		switch req.Method {
		case methodUploadScore:
			// RankingScoreData {u32 category, u32 score, u8 orderBy, u8 updateMode, List<u8> groups,
			// u64 param}, then u64 uniqueId.
			sd := readStructHeader(in)
			category, score := sd.U32(), sd.U32()
			orderBy, updateMode := sd.U8(), sd.U8()
			groups := nex.ReadList(sd, func(i *nex.StreamIn) byte { return i.U8() })
			param := sd.U64()
			uniqueID := in.U64()
			if in.Err() != nil || sd.Err() != nil {
				fmt.Printf("[Pokken Ranking] pid=%d UploadScore parse error body=%x\n", conn.PID, req.Body)
				return nex.NewRMCError(s, protocolRanking, req.CallID, nex.ResultCoreInvalidArgument)
			}
			rkMu.Lock()
			key := fmt.Sprintf("%d:%d", category, conn.PID)
			old := rk.Scores[key]
			kept := old != nil && updateMode == 0 && !better(score, old.Score, orderBy)
			if !kept {
				rk.Scores[key] = &scoreEntry{PID: conn.PID, UniqueID: uniqueID, Category: category, Score: score,
					OrderBy: orderBy, Groups: groups, Param: param, UpdatedAt: uint64(nex.NowDateTime())}
				rkSaveLocked()
			}
			rkMu.Unlock()
			fmt.Printf("[Pokken Ranking] pid=%d UploadScore category=%d score=%d orderBy=%d updateMode=%d groups=%v kept-old=%v\n",
				conn.PID, category, score, orderBy, updateMode, groups, kept)
			return rankingOK(conn, req, nil)

		case methodUploadCommonData:
			data := in.Buffer()
			_ = in.U64() // uniqueId
			if in.Err() != nil {
				return nex.NewRMCError(s, protocolRanking, req.CallID, nex.ResultCoreInvalidArgument)
			}
			rkMu.Lock()
			rk.CommonData[conn.PID] = data
			rkSaveLocked()
			rkMu.Unlock()
			fmt.Printf("[Pokken Ranking] pid=%d UploadCommonData len=%d\n", conn.PID, len(data))
			return rankingOK(conn, req, nil)

		case methodGetCommonData:
			_ = in.U64() // uniqueId
			rkMu.Lock()
			data, ok := rk.CommonData[conn.PID]
			rkMu.Unlock()
			if !ok {
				return nex.NewRMCError(s, protocolRanking, req.CallID, resultRankingNotFound)
			}
			out := nex.NewStreamOut(s)
			out.Buffer(data)
			return rankingOK(conn, req, out.Bytes())

		case methodGetRanking:
			return getRanking(conn, req, in)
		}
		resp := next(conn, req)
		fmt.Printf("[Pokken Ranking] pid=%d 0x70.%d body=%x -> core isError=%v\n", conn.PID, req.Method, req.Body, resp == nil || resp.IsError)
		return resp
	}
}

// getRanking answers GetRanking with a RankingResult {List<RankingRankData>, u32 total, DateTime}.
func getRanking(conn *nex.Connection, req *nex.RMCMessage, in *nex.StreamIn) *nex.RMCMessage {
	s := conn.Settings
	mode := in.U8()
	category := in.U32()
	op := readStructHeader(in)
	orderCalc, groupIndex, groupNum := op.U8(), op.U8(), op.U8()
	op.U8() // timeScope
	offset, length := op.U32(), op.U8()
	in.U64() // uniqueId
	target := in.PID()
	if target == 0 {
		target = conn.PID
	}

	rkMu.Lock()
	var rows []*scoreEntry
	orderBy := uint8(1)
	for _, e := range rk.Scores {
		if e.Category != category {
			continue
		}
		if groupIndex != rankingGroupIndexNoFilt && (int(groupIndex) >= len(e.Groups) || e.Groups[groupIndex] != groupNum) {
			continue
		}
		rows = append(rows, e)
		orderBy = e.OrderBy
	}
	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].Score != rows[b].Score {
			return better(rows[a].Score, rows[b].Score, orderBy)
		}
		return rows[a].UpdatedAt < rows[b].UpdatedAt
	})
	ranks := make([]uint32, len(rows))
	for i := range rows {
		ranks[i] = uint32(i + 1)
		if orderCalc == 0 && i > 0 && rows[i].Score == rows[i-1].Score {
			ranks[i] = ranks[i-1] // standard ranking (1224)
		}
	}
	total := len(rows)
	lo, hi := 0, total
	switch mode {
	case rankingModeSelf, rankingModeAroundSelf:
		self := -1
		for i, e := range rows {
			if e.PID == target {
				self = i
			}
		}
		if self < 0 {
			lo, hi = 0, 0
		} else if mode == rankingModeSelf {
			lo, hi = self, self+1
		} else {
			n := int(length)
			if n == 0 {
				n = 1
			}
			lo = max(0, self-n/2)
			hi = min(total, lo+n)
			lo = max(0, hi-n)
		}
	default:
		lo = min(int(offset), total)
		if length != 0 {
			hi = min(total, lo+int(length))
		}
	}
	out := nex.NewStreamOut(s)
	writeStruct(out, func(o *nex.StreamOut) {
		o.U32(uint32(hi - lo))
		for i := lo; i < hi; i++ {
			e := rows[i]
			writeStruct(o, func(o *nex.StreamOut) {
				o.PID(e.PID)
				o.U64(e.UniqueID)
				o.U32(ranks[i])
				o.U32(e.Category)
				o.U32(e.Score)
				nex.WriteList(o, e.Groups, func(o *nex.StreamOut, g byte) { o.U8(g) })
				o.U64(e.Param)
				o.Buffer(rk.CommonData[e.PID])
				o.DateTime(e.UpdatedAt) // updateTime: present from ranking 3.6 on (2306-0116 without)
			})
		}
		o.U32(uint32(total))
		o.DateTime(uint64(nex.NowDateTime()))
	})
	rkMu.Unlock()
	fmt.Printf("[Pokken Ranking] pid=%d GetRanking mode=%d category=%d group=%d/%d offset=%d len=%d -> rows %d..%d of %d\n",
		conn.PID, mode, category, groupIndex, groupNum, offset, length, lo, hi, total)
	return rankingOK(conn, req, out.Bytes())
}
