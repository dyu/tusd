package cli

import (
	"fmt"
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

func (sc *SyncContext) init() error {
	return sc.db.View(func(txn *badger.Txn) error {
		itOpts := badger.IteratorOptions{
			PrefetchValues: false,
			PrefetchSize:   1,
			Reverse:        true,
			AllVersions:    false,
		}
		it := txn.NewIterator(itOpts)
		it.Rewind()
		if !it.Valid() {
			return nil
		}
		
		k := it.Item().Key()
		if len(k) != 12 {
			return fmt.Errorf("Invalid key length: %d", len(k))
		}
		
		seq := binary.BigEndian.Uint64(k[4:])
		
		syncType := int(0xFF & k[3])
		
		if syncType == Flags.SyncType {
			sc.seqPush = seq
			return nil
		}
		
		sc.seqPull = seq
		
		pfx := make([]byte, 4)
		pfx[0] = 0x80
		binary.BigEndian.PutUint16(pfx[1:], uint16(Flags.SyncId))
		pfx[3] = byte(Flags.SyncType | 0x80)
		
		it.Seek(pfx)
		if !it.Valid() {
			return fmt.Errorf("Illegal db state.")
		}
		it.Next()
		if !it.Valid() {
			return fmt.Errorf("Illegal db state.")
		}
		
		k = it.Item().Key()
		if len(k) != 12 {
			return fmt.Errorf("Invalid key length: %d", len(k))
		}
		
		if k[3] != byte(Flags.SyncType) {
			return fmt.Errorf("Illegal db state.")
		}
		
		sc.seqPush = binary.BigEndian.Uint64(k[4:])
		return nil
	})
}

func (sc *SyncContext) FillSeqReq(seq uint64, pull bool) {
	sc.seqReq[0] = 0x80
	binary.BigEndian.PutUint16(sc.seqReq[1:], uint16(Flags.SyncId))
	sc.seqReq[3] = byte(Flags.SyncType)
	binary.BigEndian.PutUint64(sc.seqReq[4:], seq)
	sc.seqReq[12] = 0
}
