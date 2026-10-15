package main

// DataStore (0x73) for Pokken: the player card is posted with PostMetaBinary (21) at every login and
// after each match, and read back with GetMeta (8). Records are kept by dataId and by (owner, slot)
// and saved to POKKEN_DATASTORE_FILE so cards survive restarts. Other methods get an empty answer.

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	nex "github.com/NextendoNetwork/nextendo-nex"
)

const (
	protocolDataStore uint16 = 0x73

	methodGetMeta             uint32 = 8
	methodSearchObject        uint32 = 12
	methodPostMetaBinary      uint32 = 21
	methodGetPersistenceInfos uint32 = 30

	dataStoreNotFound uint32 = 0x80690004

	resultOptionMetaBinary uint8 = 0x4
)

type cardRecord struct {
	DataID     uint64
	OwnerID    uint64
	Slot       uint16
	Size       uint32
	Name       string
	DataType   uint16
	MetaBinary []byte
	Tags       []string
	CreatedAt  uint64
	UpdatedAt  uint64
}

type slotKey struct {
	Owner uint64
	Slot  uint16
}

var (
	dsMu     sync.Mutex
	dsByID          = map[uint64]*cardRecord{}
	dsBySlot        = map[slotKey]uint64{}
	dsNextID uint64 = 1
	dsFile          = envOr("POKKEN_DATASTORE_FILE", "pokken_datastore.json")
)

func init() {
	b, err := os.ReadFile(dsFile)
	if err != nil {
		return
	}
	var recs []*cardRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		fmt.Printf("[Pokken DataStore] %s unreadable: %v\n", dsFile, err)
		return
	}
	for _, r := range recs {
		dsByID[r.DataID] = r
		dsBySlot[slotKey{r.OwnerID, r.Slot}] = r.DataID
		if r.DataID >= dsNextID {
			dsNextID = r.DataID + 1
		}
	}
	fmt.Printf("[Pokken DataStore] loaded %d record(s) from %s\n", len(recs), dsFile)
}

// dsSaveLocked writes every record to disk; dsMu must be held.
func dsSaveLocked() {
	recs := make([]*cardRecord, 0, len(dsByID))
	for _, r := range dsByID {
		recs = append(recs, r)
	}
	b, err := json.Marshal(recs)
	if err == nil {
		err = os.WriteFile(dsFile+".tmp", b, 0o644)
	}
	if err == nil {
		err = os.Rename(dsFile+".tmp", dsFile)
	}
	if err != nil {
		fmt.Printf("[Pokken DataStore] save failed: %v\n", err)
	}
}

func dataStoreStubHandler() nex.RMCHandler {
	return func(conn *nex.Connection, req *nex.RMCMessage) *nex.RMCMessage {
		s := conn.Settings

		switch req.Method {
		case methodGetMeta:
			return dsGetMeta(conn, req)

		case methodPostMetaBinary:
			return dsPostMetaBinary(conn, req)

		case methodSearchObject:
			// Replay Theater. DataStoreSearchResult: u32 totalCount, List<DataStoreMetaInfo>,
			// u8 totalCountType. A bare empty list is too short (2306-0116).
			out := nex.NewStreamOut(s)
			writeStruct(out, func(o *nex.StreamOut) {
				o.U32(0)
				o.U32(0)
				o.U8(0)
			})
			fmt.Printf("[Pokken DataStore] pid=%d 0x73.12 SearchObject -> 0 results\n", conn.PID)
			return nex.NewRMCSuccess(s, protocolDataStore, req.Method, req.CallID, out.Bytes())

		case methodGetPersistenceInfos:
			in := nex.NewStreamIn(req.Body, s)
			_ = in.U64() // ownerId
			n := in.U32()
			if in.Err() != nil || n > 64 {
				n = 0
			}
			out := nex.NewStreamOut(s)
			out.U32(0) // pPersistenceInfo: empty (no data saved yet)
			out.U32(n) // pResults: one per requested slot
			for i := uint32(0); i < n; i++ {
				out.U32(dataStoreNotFound)
			}
			fmt.Printf("[Pokken DataStore] 0x73.30 GetPersistenceInfos slots=%d -> NotFound each\n", n)
			return nex.NewRMCSuccess(s, protocolDataStore, req.Method, req.CallID, out.Bytes())

		default:
			out := nex.NewStreamOut(s)
			out.U32(0) // empty list / count = 0
			fmt.Printf("[Pokken DataStore] pid=%d 0x73.%d bodyLen=%d body=%x -> empty list (stub)\n", conn.PID, req.Method, len(req.Body), req.Body)
			return nex.NewRMCSuccess(s, protocolDataStore, req.Method, req.CallID, out.Bytes())
		}
	}
}

// readStructHeader consumes a Structure's [u8 version][u32 length] header and returns its body.
func readStructHeader(in *nex.StreamIn) *nex.StreamIn {
	if !in.Settings.StructHeader {
		return in
	}
	in.U8()
	return in.Substream()
}

// Pokken (NEX 4.3.1) sends DataStorePermission as a Structure with its own header (measured from a
// real PostMetaBinary: 00 05000000 <u8 perm> <u32 0>); SMO's newer client sends it flat.
func readPermission(in *nex.StreamIn) {
	p := readStructHeader(in)
	p.U8()
	nex.ReadList(p, func(in *nex.StreamIn) uint64 { return in.PID() })
}

func writePermission(out *nex.StreamOut) {
	writeStruct(out, func(o *nex.StreamOut) {
		o.U8(0) // public
		nex.WriteList(o, []uint64{}, func(o *nex.StreamOut, v uint64) { o.PID(v) })
	})
}

func writeStruct(out *nex.StreamOut, body func(*nex.StreamOut)) {
	if !out.Settings.StructHeader {
		body(out)
		return
	}
	sub := nex.NewStreamOut(out.Settings)
	body(sub)
	out.U8(0)
	out.Buffer(sub.Bytes())
}

// dsPostMetaBinary stores DataStorePreparePostParam's meta binary; the answer is a bare u64 dataId.
func dsPostMetaBinary(conn *nex.Connection, req *nex.RMCMessage) *nex.RMCMessage {
	s := conn.Settings
	in := readStructHeader(nex.NewStreamIn(req.Body, s))
	size := in.U32()
	name := in.String()
	dataType := in.U16()
	meta := in.QBuffer()
	readPermission(in) // permission
	readPermission(in) // delPermission
	in.U32()           // flag
	in.U16()           // period
	in.U32()           // referDataId
	tags := nex.ReadList(in, func(in *nex.StreamIn) string { return in.String() })
	if in.Err() != nil {
		fmt.Printf("[Pokken DataStore] pid=%d PostMetaBinary parse error %v body=%x\n", conn.PID, in.Err(), req.Body)
		return nex.NewRMCError(s, protocolDataStore, req.CallID, nex.ResultCoreInvalidArgument)
	}
	// ratingInitParams then persistenceInitParam (slot first); parsed on a copy so a layout
	// mismatch only costs the slot (0), never the post.
	rest := nex.NewStreamIn(in.Read(in.Remaining()), s)
	nex.ReadList(rest, func(in *nex.StreamIn) bool { readStructHeader(in); return true })
	slot := readStructHeader(rest).U16()
	if rest.Err() != nil {
		slot = 0
	}

	now := uint64(nex.NowDateTime())
	dsMu.Lock()
	key := slotKey{conn.PID, slot}
	id, replaced := dsBySlot[key]
	if !replaced {
		id = dsNextID
		dsNextID++
	}
	created := now
	if old := dsByID[id]; old != nil {
		created = old.CreatedAt
	}
	dsByID[id] = &cardRecord{DataID: id, OwnerID: conn.PID, Slot: slot, Size: size, Name: name, DataType: dataType,
		MetaBinary: meta, Tags: tags, CreatedAt: created, UpdatedAt: now}
	dsBySlot[key] = id
	dsSaveLocked()
	dsMu.Unlock()

	out := nex.NewStreamOut(s)
	out.U64(id)
	fmt.Printf("[Pokken DataStore] pid=%d PostMetaBinary slot=%d type=%d metaLen=%d -> dataId=%d (updated=%v)\n",
		conn.PID, slot, dataType, len(meta), id, replaced)
	return nex.NewRMCSuccess(s, protocolDataStore, req.Method, req.CallID, out.Bytes())
}

// dsGetMeta answers DataStoreGetMetaParam: by dataId, or by persistenceTarget (owner, slot) when it is 0.
func dsGetMeta(conn *nex.Connection, req *nex.RMCMessage) *nex.RMCMessage {
	s := conn.Settings
	in := readStructHeader(nex.NewStreamIn(req.Body, s))
	dataID := in.U64()
	pt := readStructHeader(in)
	owner := pt.PID()
	slot := pt.U16()
	resultOption := in.U8()
	if in.Err() != nil {
		fmt.Printf("[Pokken DataStore] pid=%d GetMeta parse error %v body=%x\n", conn.PID, in.Err(), req.Body)
		return nex.NewRMCError(s, protocolDataStore, req.CallID, nex.ResultCoreInvalidArgument)
	}

	dsMu.Lock()
	if dataID == 0 {
		dataID = dsBySlot[slotKey{owner, slot}]
	}
	r := dsByID[dataID]
	dsMu.Unlock()
	if r == nil {
		fmt.Printf("[Pokken DataStore] pid=%d GetMeta dataId=%d owner=%d slot=%d -> NotFound\n", conn.PID, dataID, owner, slot)
		return nex.NewRMCError(s, protocolDataStore, req.CallID, dataStoreNotFound)
	}

	out := nex.NewStreamOut(s)
	writeMetaInfo(out, r, resultOption&resultOptionMetaBinary != 0)
	fmt.Printf("[Pokken DataStore] pid=%d GetMeta dataId=%d owner=%d slot=%d option=%#x -> metaLen=%d\n",
		conn.PID, r.DataID, r.OwnerID, r.Slot, resultOption, len(r.MetaBinary))
	return nex.NewRMCSuccess(s, protocolDataStore, req.Method, req.CallID, out.Bytes())
}

// writeMetaInfo writes a DataStoreMetaInfo; metaBinary is only on the wire when it was asked for.
func writeMetaInfo(out *nex.StreamOut, r *cardRecord, withMeta bool) {
	writeStruct(out, func(o *nex.StreamOut) {
		o.U64(r.DataID)
		o.PID(r.OwnerID)
		o.U32(r.Size)
		o.String(r.Name)
		o.U16(r.DataType)
		if withMeta {
			o.QBuffer(r.MetaBinary)
		}
		writePermission(o)
		writePermission(o)
		o.DateTime(r.CreatedAt)
		o.DateTime(r.UpdatedAt)
		o.U16(0)      // period
		o.U8(0)       // status
		o.U32(0)      // referredCnt
		o.U32(0)      // referDataId
		o.U32(0)      // flag
		o.DateTime(0) // referredTime
		o.DateTime(0) // expireTime
		nex.WriteList(o, r.Tags, func(o *nex.StreamOut, t string) { o.String(t) })
		nex.WriteList(o, []int8{}, func(o *nex.StreamOut, v int8) {}) // ratings
	})
}
