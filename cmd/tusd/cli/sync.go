package cli

import (
	"bytes"
	"fmt"
	"encoding/binary"
	
	"log"
	"net/url"
	"os"
	"os/signal"
	"time"

	"github.com/dgraph-io/badger"
	"github.com/golang/snappy"
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

func fillPushKey(key []byte, seq uint64) []byte {
	key[0] = 0x80
	binary.BigEndian.PutUint16(key[1:], uint16(Flags.SyncId))
	key[3] = byte(Flags.SyncType)
	binary.BigEndian.PutUint64(key[4:], seq)
	return key
}

func PutSyncEntry(key []byte, sc *SyncContext) error {
	return sc.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, []byte{0})
	})
}

func UpdateSyncEntry(key []byte, sc *SyncContext) error {
	err := sc.db.Update(func(txn *badger.Txn) error {
		err := txn.Set(key, []byte{1})
		if err == nil {
			return err
		}
		
		return txn.Set(fillPushKey(make([]byte, 12), sc.seqPush + 1), key)
	})
	
	if err == nil {
		sc.seqPush += 1
	}
	
	return err
}

func handlePayload(data []byte, sc *SyncContext) (err error) {
	// TODO
	return err
}

func sendPushEntry(message []byte, seq uint64, sc *SyncContext) error {
	var buf bytes.Buffer
	key := message[0:len(message) - 1]
	
	err := sc.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}
		val, err := item.Value()
		if err != nil {
			return err
		}
		
		if len(val) != 9 {
			return fmt.Errorf("Invalid val length: %d != 9", len(val))
		}
		
		buf.Write(message[0:4]) // header
		buf.Write(key)
		buf.Write(val)
		return err
	})
	
	if err != nil {
		return err
	}
	
	buf.WriteByte(2) // push payload type
	payload := buf.Bytes()
	
	binary.BigEndian.PutUint16(payload, uint16(len(key)))
	binary.BigEndian.PutUint16(payload[2:], 9)
	
	return sc.c.WriteMessage(2, payload)
}

func closeConnection(sc *SyncContext) {
	c := sc.c
	sc.c = nil
	c.Close()
}

func handleConnection(sc *SyncContext) {
	defer closeConnection(sc)
	//defer close(done)
	
	err := sc.c.WriteMessage(2, sc.seqReq[0:])
	if err != nil {
		return
	}
	
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
		last := message[messageLen - 1]
		if 0 == last {
			if messageLen == 13 {
				// send the push entry with the provided key
				seq = binary.BigEndian.Uint64(message[4:])
				if sc.seqPush >= seq {
					_ = sendPushEntry(message, seq, sc)
				}
				continue
			}
			
			// received a pub message
			if messageLen != 5 || 0x80 != message[0] {
				log.Printf("Received a invalid pub message %d\n", messageLen)
				continue
			}
			
			if 0xFF == message[1] && 0xFF == message[2] && pullType == int(0xFF & message[3]) {
				// send current seq
				_  = sc.c.WriteMessage(2, sc.seqReq[0:])
			}
			
			continue
		} else if 0xFF != last {
			log.Printf("Received a invalid message %d\n", messageLen)
			continue
		} else if (1 != message[messageLen - 2]) {
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
		go handleConnection(sc)
	}
	
	ticker := time.NewTicker(time.Duration(Flags.SyncInterval) * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		//case t := <-ticker.C:
		case <-ticker.C:
			if sc.c != nil {
				if sc.sendOnNextTick {
					sc.sendOnNextTick = false
					sc.c.WriteMessage(2, sc.seqReq[0:])
				}
				continue
			}
			c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
			if err == nil {
				sc.c = c
				go handleConnection(sc)
			}
		case <-interrupt:
			//log.Println("interrupt")
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


