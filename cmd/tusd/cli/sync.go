package cli

import (
	"encoding/binary"

	"github.com/dgraph-io/badger"
	"github.com/gorilla/websocket"
)

var dbuf []byte = make([]byte, 0x1FFFF)

type SyncContext struct {
	c              *websocket.Conn
	db             *badger.DB
	tsVal          [8]byte
	seqReq         [13]byte
	seqPull        uint64
	seqPush        uint64
	sendOnNextTick bool
}

func (sc *SyncContext) FillSeqReq(seq uint64, pull bool) {
	sc.seqReq[0] = 0x80
	binary.BigEndian.PutUint16(sc.seqReq[1:], uint16(Flags.SyncId))
	sc.seqReq[3] = byte(Flags.SyncType)
	binary.BigEndian.PutUint64(sc.seqReq[4:], seq)
	sc.seqReq[12] = 0
}
