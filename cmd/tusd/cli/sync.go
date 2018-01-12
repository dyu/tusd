package cli

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"log"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/badger"
	"github.com/golang/snappy"
	"github.com/gorilla/websocket"
)

var dbuf []byte = make([]byte, 0x1FFFF)
var zeroVal = []byte("0")

type SyncContext struct {
	c              *websocket.Conn
	db             *badger.DB
	tsVal          [8]byte
	seqReq         [13]byte
	sendOnNextTick bool
	seqPull        uint64
	seqPush        uint64
	ackedPush      uint64 // atomic
	sentPush       uint64 // atomic
	ticksPushRetry int
	chanPush       chan bool
	chanClose      chan bool
	muxUpdate      sync.Mutex
}

func (sc *SyncContext) initSeqReq() {
	sc.seqReq[0] = 0x80
	binary.BigEndian.PutUint16(sc.seqReq[1:], uint16(Flags.SyncId))
	sc.seqReq[3] = byte(Flags.SyncType | 0x80)
	binary.BigEndian.PutUint64(sc.seqReq[4:], sc.seqPull+1)
	sc.seqReq[12] = 0
}

func fillPushKey(key []byte, seq uint64) []byte {
	key[0] = 0x80
	binary.BigEndian.PutUint16(key[1:], uint16(Flags.SyncId))
	key[3] = byte(Flags.SyncType)
	binary.BigEndian.PutUint64(key[4:], seq)
	return key
}

func PutSyncEntry(key []byte, sc *SyncContext) error {
	return sc.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, zeroVal)
	})
}

func UpdateSyncEntry(key []byte, id string, sc *SyncContext) error {
	sc.muxUpdate.Lock()
	defer sc.muxUpdate.Unlock()

	seq := atomic.LoadUint64(&sc.seqPush) + 1

	err := sc.db.Update(func(txn *badger.Txn) error {
		idBytes := []byte(id)
		err := txn.Set(key, idBytes)
		if err != nil {
			return err
		}

		return txn.Set(fillPushKey(make([]byte, 12), seq), append(idBytes, key...))
	})

	if err == nil {
		atomic.StoreUint64(&sc.seqPush, seq)
		sc.chanPush <- true
	}

	return err
}

func sendPushEntry(message []byte, seq uint64, sc *SyncContext) error {
	var buf bytes.Buffer
	key := message[0 : len(message)-1]
	
	var valLen int

	err := sc.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}
		val, err := item.Value()
		if err != nil {
			return err
		}
		
		valLen = len(val)
		
		buf.Write(message[0:4]) // header
		buf.Write(key)
		buf.Write(val)
		return err
	})

	if err != nil {
		return err
	}

	buf.WriteByte(3) // push payload type
	payload := buf.Bytes()

	binary.LittleEndian.PutUint16(payload, uint16(len(key)))
	binary.LittleEndian.PutUint16(payload[2:], uint16(valLen))

	err = sc.c.WriteMessage(2, payload)

	if err == nil {
		atomic.StoreUint64(&sc.sentPush, seq)
	}

	return err
}

func handlePayload(data []byte, sc *SyncContext) (err error) {
	txn := sc.db.NewTransaction(true)
	defer txn.Discard()

	count := binary.LittleEndian.Uint16(data)
	i := uint16(0)
	klen := uint16(0)
	vlen := uint16(0)
	offset := uint16(2)
	var failedIdx int = -1
	var failedKey []byte

	for ; i < count; i++ {
		klen = binary.LittleEndian.Uint16(data[offset:])
		offset += 2
		vlen = binary.LittleEndian.Uint16(data[offset:])
		offset += 2

		key := data[offset : offset+klen]
		offset += klen
		val := data[offset : offset+vlen]
		offset += vlen

		syncKeyItem, err := txn.Get(val)
		if err != nil {
			failedIdx = int(i)
			failedKey = key
			break
		}

		syncKey, err := syncKeyItem.Value()
		if err != nil {
			failedIdx = int(i)
			failedKey = key
			break
		}

		syncUidItem, err := txn.Get(syncKey)
		if err != nil {
			failedIdx = int(i)
			failedKey = key
			break
		}

		syncUid, err := syncUidItem.Value()
		if err != nil {
			failedIdx = int(i)
			failedKey = key
			break
		}

		prefix := Flags.UploadDir + "/" + string(syncUid)
		binPath := prefix + ".bin"
		infoPath := prefix + ".info"

		_, err = os.Stat(binPath)
		if os.IsNotExist(err) {
			// noop
		} else if nil != os.Remove(binPath) {
			failedIdx = int(i)
			failedKey = key
			err = fmt.Errorf("Could not remove bin: %s", binPath)
			break
		}

		_, err = os.Stat(infoPath)
		if os.IsNotExist(err) {
			// noop
		} else if nil != os.Remove(infoPath) {
			failedIdx = int(i)
			failedKey = key
			err = fmt.Errorf("Could not remove info: %s", infoPath)
			break
		}

		err = txn.Set(key, val)
		if err != nil {
			failedIdx = int(i)
			failedKey = key
			break
		}
	}

	if failedIdx != -1 {
		log.Println("Failed on write | key:", hex.Dump(failedKey),
			"| seq:", binary.BigEndian.Uint64(failedKey[4:]))
		return err
	}

	if i == 0 {
		return err
	}

	errCommit := txn.Commit(nil)
	if errCommit != nil {
		log.Println("Email sent but failed on commit:", errCommit, "| key:", hex.Dump(failedKey),
			"| seq:", binary.BigEndian.Uint64(failedKey[4:]))
		return errCommit
	}

	sc.seqPull += uint64(i)
	binary.BigEndian.PutUint64(sc.seqReq[4:], sc.seqPull+1)

	if err == nil {
		// send next batch
		sc.chanPush <- false
	}

	return err
}

func closeConnection(sc *SyncContext) {
	sc.chanClose <- true
}

func handleConnection(sc *SyncContext) {
	defer closeConnection(sc)
	//defer close(done)

	pullType := Flags.SyncType | 0x80
	var seq uint64

	for {
		typ, message, err := sc.c.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, 1000) {
				log.Println("read:", err)
			}
			return
		}
		if typ == 8 {
			// closed
			return
		}

		messageLen := len(message)
		last := message[messageLen-1]
		if 0 == last {
			if messageLen == 13 {
				// send the push entry with the provided key
				seq = binary.BigEndian.Uint64(message[4:])
				atomic.StoreUint64(&sc.ackedPush, seq-1)
				if seq <= atomic.LoadUint64(&sc.seqPush) {
					sc.chanPush <- true
				}
				continue
			}

			// received a pub message
			if messageLen != 5 || 0x80 != message[0] {
				log.Printf("Received a invalid pub message %d\n", messageLen)
				continue
			}

			if 0xFF == message[1] && 0xFF == message[2] && pullType == int(0xFF&message[3]) {
				// send current seq
				sc.chanPush <- false
			}
			continue
		} else if 0xFF != last {
			log.Printf("Received a invalid message %d\n", messageLen)
			continue
		} else if 1 != message[messageLen-2] {
			log.Printf("Received a non-pullsync message %d\n", messageLen)
			continue
		}

		decoded, err := snappy.Decode(dbuf, message[0:messageLen-2])
		if err != nil {
			log.Printf("%v\n", err)
			continue
		}

		sc.sendOnNextTick = nil != handlePayload(decoded, sc)
	}
}

func loopConnect(sc *SyncContext) {
	var seq, seqSent uint64
	pushMessage := make([]byte, 13)
	pushMessage[12] = 0

	sc.initSeqReq()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	var scheme string
	if Flags.SyncSecure {
		scheme = "wss"
	} else {
		scheme = "ws"
	}

	u := url.URL{Scheme: scheme, Host: Flags.SyncAddr, Path: Flags.SyncUri}

	c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err == nil {
		sc.c = c
		_ = sc.c.WriteMessage(2, fillPushKey(pushMessage, sc.seqPull+1))
		go handleConnection(sc)
	}

	ticker := time.NewTicker(time.Duration(Flags.SyncInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sc.chanClose:
			sc.c.Close()
			sc.c = nil
		case b := <-sc.chanPush:
			if sc.c == nil {
				// noop
			} else if !b {
				_ = sc.c.WriteMessage(2, sc.seqReq[0:])
			} else {
				seqSent = atomic.LoadUint64(&sc.sentPush)
				seq = atomic.LoadUint64(&sc.ackedPush) + 1
				if seq > seqSent && nil == sendPushEntry(fillPushKey(pushMessage, seq), seq, sc) {
					sc.ticksPushRetry = 0
				}
			}
		//case t := <-ticker.C:
		case <-ticker.C:
			if sc.c != nil {
				if sc.sendOnNextTick {
					sc.sendOnNextTick = false
					_ = sc.c.WriteMessage(2, sc.seqReq[0:])
				}

				sc.ticksPushRetry += 1
				if sc.ticksPushRetry >= Flags.SyncPushRetryTicks {
					sc.ticksPushRetry = 0
					seqSent = atomic.LoadUint64(&sc.sentPush)
					seq = atomic.LoadUint64(&sc.ackedPush) + 1
					if seq == seqSent {
						// resend
						_ = sendPushEntry(fillPushKey(pushMessage, seq), seq, sc)
					}
				}
				continue
			}

			sc.ticksPushRetry = 0
			c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
			if err == nil {
				sc.c = c
				_ = sc.c.WriteMessage(2, fillPushKey(pushMessage, sc.seqPull+1))
				go handleConnection(sc)
			}
		case <-interrupt:
			// To cleanly close a connection, a client should send a close
			// frame and wait for the server to close the connection.
			err := c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			if err != nil {
				//log.Println("write close:", err)
				return
			}
			select {
			case <-time.After(time.Second):
			}
			os.Exit(0)
			return
		}
	}
}

func (sc *SyncContext) init() error {
	err := sc.db.View(func(txn *badger.Txn) error {
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
		
		if len(k) == 9 {
			// no sync message received yet
			return nil
		}
		
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

	if err == nil {
		go loopConnect(sc)
	}

	return err
}
