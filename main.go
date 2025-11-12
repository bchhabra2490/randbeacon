package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/sha3"
)

const (
	TickInterval = 1000 * time.Millisecond
	EntropyChanSize = 32768
	HistorySize = 4096
	MaxSnapshotStore = 8192
	PublicEntropyLen = 64
	FinalRandonLen = 64
	MaxUserDeriveLen = 8192
	LocalSecretPath = "local_secret.bin"
	LocalSecretSize = 32
	MarketConnRetryBack = 2 * time.Second
)

var (
	BinanceWS = "wss://stream.binance.com:9443/ws/%s@trade"
	CoinbaseWS = "wss://ws-feed.exchange.coinbase.com"
	KrakenWS = "wss://ws.kraken.com"
	DefaultPairs = []string{"btcusdt", "ethusdt"}
	CoinbasePairs = []string{"BTC-USD", "ETH-USD"}
	KrakenPairs = []string{"XBT/USD", "ETH/USD"}
)


type EntropyEvent struct {
	Source string
	Ts int64
	Bytes []byte
}

type BeaconEntry struct {
	Index uint64 `json:"index"`
	Timestamp int64 `json:"timestamp_ns"`
	PublicEntropyHex string `json:"public_entropy_hex"`
	FinalRandomHex string `json:"final_random_hex"`
	SnapshotID string `json:"snapshot_id"`
}

var (
	entropyCh = make(chan EntropyEvent, EntropyChanSize)

	historyMu sync.RWMutex
	history =  make([]BeaconEntry, 0, HistorySize)
	latestPtr atomic.Value

	snapshotsMu sync.RWMutex
	snapshots = map[uint64][]byte{}

	bufPool = sync.Pool{New: func() any { b := make([]byte, 0, 4096); return &b }}
	localSecret []byte
)

var (
	bufPool2 = sync.Pool{New: func() any { return bytes.NewBuffer(make([]byte, 0, 32*1024));}}

	curBufMu sync.Mutex
	curBuf *bytes.Buffer

	eventsThisRound uint64
	bytesThisRound uint64
)

func initAggBuffers(){
	curBuf = bufPool2.Get().(*bytes.Buffer)
	curBuf.Reset()
}

func nowNS() int64 {return time.Now().UnixNano()}

func appendHistory(entry BeaconEntry){
	historyMu.Lock()
	defer historyMu.Unlock()

	if len(history) >= HistorySize {
		history = history[1:]
	}
	history = append(history, entry)
	latestPtr.Store(&entry)
}

func getHistory(n int) []BeaconEntry{
	historyMu.RLock()
	defer historyMu.RUnlock()

	total := len(history)
	if n > total {
		n = total
	}

	out := make([]BeaconEntry, n)
	copy(out, history[total-n:])
	return out
}

func compactEventBytes(ev EntropyEvent) []byte{
	p := bufPool.Get().(*[]byte)
	b := *p
	b = b[:0]
	b = append(b, ev.Source...)
	b = append(b, '|')
	b = append(b, []byte(strconv.FormatInt(ev.Ts, 10))...)
	b = append(b, '|')
	b = append(b, ev.Bytes...)

	out := make([]byte, len(b))
	copy(out, b)

	*p = b
	bufPool.Put(p)
	return out
}

func shortSnapshotID(b []byte) string{
	h := sha3.New512()
	_, _ = h.Write(b)
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

func loadOrCreateLocalSecret() ([]byte, error){
	if _, err := os.Stat(LocalSecretPath); err == nil{
		b, err := os.ReadFile(LocalSecretPath)
		if err != nil{
			return nil, err
		}

		if len(b) >= LocalSecretSize {
			return b[:LocalSecretSize], nil
		}
	}

	b := make([]byte, LocalSecretSize)
	f, err := os.OpenFile(LocalSecretPath, os.O_CREATE|os.O_WRONLY, 0o600)

	if err != nil{
		return nil, err
	}
	defer f.Close()

	if _, err := io.ReadFull(os.Stdin, b); err != nil{
		r, err2 := os.Open("/dev/urandom")
		if err2 == nil{
			defer r.Close()
			_, _ = io.ReadFull(r, b)
		}else{
			tmp := sha3.Sum512([]byte(fmt.Sprint("%d-%d", time.Now().UnixNano(), os.Getpid())))
			copy(b, tmp[:LocalSecretSize])
		}
	}

	_, _ = f.Write(b)
	return b, nil
}

func wsReadWithTimeout(conn *websocket.Conn, timeout time.Duration) ([]byte, error){
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	_, msg, err := conn.ReadMessage()
	return msg, err
}

func binanceCollector(ctx context.Context, symbol string){
	url := fmt.Sprintf(BinanceWS, symbol)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil{
			fmt.Printf("[binance %s] dial err: %v\n", symbol, err)
			time.Sleep(MarketConnRetryBack)
			continue
		}

		fmt.Printf("[binance %s] connected\n", symbol)
		conn.SetReadLimit(1 << 20)
		for {
			msg, err := wsReadWithTimeout(conn, 30*time.Second)
			if err != nil{
				fmt.Printf("[binance %s] read err: %v\n", symbol, err)
				conn.Close()
				break
			}

			ev := EntropyEvent{Source: "binance:"+symbol, Ts: nowNS(), Bytes: msg}
			select{
			case entropyCh<-ev:
			default:
				select{
				case <-entropyCh:
				default:
				}
				select{
				case entropyCh<-ev:
				default:
				}
			}
		}	
		
		time.Sleep(MarketConnRetryBack)
	}
}

func coinbaseCollector(ctx context.Context, products []string){
	for{
		select{
		case <-ctx.Done():
			return
		default:
		}

		conn, _, err := websocket.DefaultDialer.Dial(CoinbaseWS, nil)
		if err != nil{
			fmt.Printf("[coinbase] dial err: %v\n", err)
			time.Sleep(MarketConnRetryBack)
			continue
		}

		fmt.Println("[coinbase] connected")
		sub := map[string]any{"type": "subscribe", "product_ids": products, "channels": []string{"matches"}}
		_ = conn.WriteJSON(sub)
		for{
			msg, err := wsReadWithTimeout(conn, 30*time.Second)
			if err != nil{
				fmt.Printf("[coinbase] read err: %v\n", err)
				conn.Close()
				break
			}

			ev := EntropyEvent{Source: "coinbase", Ts: nowNS(), Bytes: msg}
			select {
			case entropyCh <- ev:
			default:
				select{
				case <-entropyCh:
				default:
				}
				select{
				case entropyCh <-ev:
				default:
				}
			}
		}
		time.Sleep(MarketConnRetryBack)
	}
}

func krakenCollector(ctx context.Context, pairs []string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		conn, _, err := websocket.DefaultDialer.Dial(KrakenWS, nil)
		if err != nil {
			fmt.Printf("[kraken] dial err: %v\n", err)
			time.Sleep(MarketConnRetryBack)
			continue
		}
		fmt.Println("[kraken] connected")
		req := map[string]any{"event": "subscribe", "pair": pairs, "subscription": map[string]any{"name": "trade"}}
		_ = conn.WriteJSON(req)
		for {
			msg, err := wsReadWithTimeout(conn, 30*time.Second)
			if err != nil {
				fmt.Printf("[kraken] read err: %v\n", err)
				conn.Close()
				break
			}
			ev := EntropyEvent{Source: "kraken", Ts: nowNS(), Bytes: msg}
			select {
			case entropyCh <- ev:
			default:
				select {
				case <-entropyCh:
				default:
				}
				select {
				case entropyCh <- ev:
				default:
				}
			}
		}
		time.Sleep(MarketConnRetryBack)
	}
}


func aggregator(ctx context.Context, startPrev []byte){
	prevPublic := make([]byte, PublicEntropyLen)
	if len(startPrev) > PublicEntropyLen{
		copy(prevPublic, startPrev[:PublicEntropyLen])
	}else{
		tmp := sha3.Sum512([]byte(fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())))
		copy(prevPublic, tmp[:PublicEntropyLen])
	}

	var index uint64 = 0
	ticker := time.NewTicker(TickInterval)
	defer ticker.Stop()

	type workItem struct{
		snapshot []byte
		prevPub []byte
		ts int64
		idx uint64
	}
	workCh := make(chan workItem, 8)

	workers := runtime.NumCPU()
	for i:= 0; i<workers; i++{
		go func(){
			for wi := range workCh{
				hpub := sha3.New512()
				_, _ = hpub.Write(wi.snapshot)
				_, _ = hpub.Write(wi.prevPub)
				_, _ = hpub.Write([]byte(strconv.FormatInt(wi.ts, 10)))
				publicSum := hpub.Sum(nil)

				mac := hmac.New(sha256.New, localSecret)
				_, _ = mac.Write(publicSum)
				privateSum := mac.Sum(nil)

				hfinal := sha3.New512()
				_, _ = hfinal.Write(publicSum)
				_, _ = hfinal.Write(privateSum)
				finalSum := hfinal.Sum(nil)

				entry := BeaconEntry{
					Index: wi.idx,
					Timestamp: wi.ts,
					PublicEntropyHex: hex.EncodeToString(publicSum),
					FinalRandomHex: hex.EncodeToString(finalSum),
					SnapshotID: shortSnapshotID(wi.snapshot),
				}
				
				snapshotsMu.Lock()
				snapshots[wi.idx] = wi.snapshot
				if len(snapshots) > MaxSnapshotStore {
					for k := range snapshots {
						delete(snapshots, k)
						break
					}
				}
				snapshotsMu.Unlock()
				appendHistory(entry)
				
				atomic.AddUint64(&eventsThisRound, 0)
			}
		}()
	}	

	for {
		select{
		case <-ctx.Done():
			return
		case ev := <-entropyCh:
			packed := compactEventBytes(ev)
			curBufMu.Lock()
			curBuf.Write(packed)
			curBufMu.Unlock()
			
			atomic.AddUint64(&eventsThisRound, 1)
			atomic.AddUint64(&bytesThisRound, uint64(len(packed)))
		case <-ticker.C:
			newBuf := bufPool2.Get().(*bytes.Buffer)
			newBuf.Reset()
			
			curBufMu.Lock()
			old := curBuf
			curBuf = newBuf

			roundEvents := atomic.SwapUint64(&eventsThisRound, 0)
			roundBytes := atomic.SwapUint64(&bytesThisRound, 0)
			curBufMu.Unlock()

			snap := make([]byte, old.Len())
			copy(snap, old.Bytes())
			old.Reset()
			bufPool2.Put(old)

			ts := nowNS()
			index++

			wi := workItem{snapshot: snap, prevPub: append([]byte(nil), prevPublic...), ts: ts, idx: index}

			select{
			case workCh <- wi:
			default:
				go func(w workItem){ workCh <- w }(wi)
			}

			hpub := sha3.New512()
			_, _ = hpub.Write(snap)
			_, _ = hpub.Write(prevPublic)
			_, _ = hpub.Write([]byte(strconv.FormatInt(ts, 10)))
			publicSum := hpub.Sum(nil)

			copy(prevPublic, publicSum)

			fmt.Printf("[beacon] idx=%d ts=%d snapshot_id=%s pub=%s final=%s round_events=%d round_bytes=%d\n",
				index, ts, shortSnapshotID(snap), hex.EncodeToString(publicSum[:16]), hex.EncodeToString(publicSum[:16]), roundEvents, roundBytes)
		}
	}
}

type deriveRequest struct{
	Salt string `json:"salt"`
	Len int `json:"len"`
}

func hkdfDerive(ikm []byte, salt []byte, info []byte, l int)([]byte, error){
	if l <= 0 || l > MaxUserDeriveLen{
		return nil, fmt.Errorf("invalid length")
	}

	k := hkdf.New(sha256.New, ikm, salt, info)

	out := make([]byte, l)
	if _, err := io.ReadFull(k, out); err != nil{
		return nil, err
	}

	return out, nil
}

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {return true}}

func apiRoutes(r *gin.Engine) {
	r.GET("/", func(c *gin.Context) {
		c.File("index.html")
	})

	r.GET("/docs", func(c *gin.Context) {
		c.File("docs.html")
	})

	r.GET("/random/latest", func(c *gin.Context) {
		le := latestPtr.Load()
		if le == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no beacon yet"})
			return
		}
		c.JSON(200, le.(*BeaconEntry))
	})

	r.GET("/random/history", func(c *gin.Context) {
		nq := c.Query("n")
		n := 10
		if nq != "" {
			if v, err := strconv.Atoi(nq); err == nil && v > 0 {
				n = v
			}
		}
		c.JSON(200, getHistory(n))
	})

	r.GET("/random/snapshot/:index", func(c *gin.Context) {
		qi := c.Param("index")
		idx, err := strconv.ParseUint(qi, 10, 64)
		if err != nil {
			c.JSON(400, gin.H{"error": "invalid index"})
			return
		}
		snapshotsMu.RLock()
		snap, ok := snapshots[idx]
		snapshotsMu.RUnlock()
		if !ok {
			c.JSON(404, gin.H{"error": "snapshot not available"})
			return
		}
		c.Data(200, "application/octet-stream", snap)
	})

	r.POST("/random/derive", func(c *gin.Context) {
		var req deriveRequest
		if err := c.BindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if req.Len <= 0 || req.Len > MaxUserDeriveLen {
			c.JSON(400, gin.H{"error": "invalid len"})
			return
		}
		le := latestPtr.Load()
		if le == nil {
			c.JSON(503, gin.H{"error": "no beacon yet"})
			return
		}
		entry := le.(*BeaconEntry)
		finalBytes, err := hex.DecodeString(entry.FinalRandomHex)
		if err != nil {
			c.JSON(500, gin.H{"error": "internal decode error"})
			return
		}
		var salt []byte
		if bs, err := hex.DecodeString(req.Salt); err == nil && len(bs) > 0 {
			salt = bs
		} else {
			salt = []byte(req.Salt)
		}
		out, err := hkdfDerive(finalBytes, salt, []byte("randforge-derive-v1"), req.Len)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{
			"derived_hex": hex.EncodeToString(out),
			"len":         len(out),
			"index":       entry.Index,
			"timestamp":   entry.Timestamp,
			"snapshot_id": entry.SnapshotID,
		})
	})

	r.GET("/stream", func(c *gin.Context) {
		w := c.Writer
		req := c.Request
		conn, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if le := latestPtr.Load(); le != nil {
			_ = conn.WriteJSON(le.(*BeaconEntry))
		}
		t := time.NewTicker(TickInterval)
		defer t.Stop()
		for {
			select {
			case <-req.Context().Done():
				return
			case <-t.C:
				if le := latestPtr.Load(); le != nil {
					if err := conn.WriteJSON(le.(*BeaconEntry)); err != nil {
						return
					}
				}
			}
		}
	})
}

func main() {
	// load/create local secret
	ls, err := loadOrCreateLocalSecret()
	if err != nil {
		fmt.Printf("failed to load/create local secret: %v\n", err)
		os.Exit(1)
	}
	localSecret = ls
	fmt.Println("local secret loaded")

	// gin
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	apiRoutes(router)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// start collectors
	for _, p := range DefaultPairs {
		go binanceCollector(ctx, p)
	}
	go coinbaseCollector(ctx, CoinbasePairs)
	go krakenCollector(ctx, KrakenPairs)

	// seed prevPublic from /dev/urandom
	seed := make([]byte, PublicEntropyLen)
	rf, err := os.Open("/dev/urandom")
	if err == nil {
		_, _ = io.ReadFull(rf, seed)
		rf.Close()
	} else {
		tmp := sha3.Sum512([]byte(fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())))
		copy(seed, tmp[:])
	}

	initAggBuffers()
	go aggregator(ctx, seed)

	srv := &http.Server{
		Addr:    ":8080",
		Handler: router,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("server err: %v\n", err)
			cancel()
		}
	}()

	fmt.Println("RandForge (hybrid) running on :8080")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	fmt.Println("shutting down...")
	cancel()
	_ = srv.Shutdown(context.Background())
}