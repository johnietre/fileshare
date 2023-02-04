// TODO: Compress files over a certain size
// TODO: Add file uploads/folder creation/deletion
// TODO: Add auto-updates
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
  "sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	srvrMap                                  sync.Map
	broadcastAddr                            *net.UDPAddr
	thisSrvrBytes                            []byte
	fsDir, tempDir, homeTempPath, fsTempPath string
	homeTsVal, fsTsVal                       atomic.Value
	msgChan                                  = make(chan MsgInfo, 10)
)

func main() {
	log.SetFlags(0)

	var (
		broadcastIP, udpIP, srvrIP, name, optsPath string
		broadcastPort, udpPort, srvrPort           int
	)

	flag.StringVar(&broadcastIP, "broadcast-ip", "", "IPv4 address to broadcast to (no port)")
	flag.IntVar(&broadcastPort, "broadcast-port", 55555, "Port to broadcast to")
	flag.StringVar(&udpIP, "udp-ip", "0.0.0.0", "IPv4 address to bind UDP to (no port)")
	flag.IntVar(&udpPort, "udp-port", 55555, "Port to bind UDP to")
	flag.StringVar(&srvrIP, "srvr-ip", "", "IPv4 address to run server on (no port)")
	flag.IntVar(&srvrPort, "srvr-port", 5555, "Port to run server on")
	flag.StringVar(&name, "name", "", "Name of server")
	flag.StringVar(&fsDir, "dir", ".", "Directory to serve")
	flag.StringVar(&tempDir, "temp-path", ".", "Path to HTML template file")
	flag.StringVar(&optsPath, "opts-path", "", "Path to options JSON file")
	flag.Parse()

  if name == "" {
    fatalerr("must provide name")
  }

	homeTempPath = filepath.Join(tempDir, "index.html")
	ts, err := template.ParseFiles(homeTempPath)
	if err != nil {
		fatalerr("error parsing HTML template file:", err)
	}
	homeTsVal.Store(ts)
	fsTempPath = filepath.Join(tempDir, "fs.html")
	if ts, err = template.ParseFiles(fsTempPath); err != nil {
		fatalerr("error parsing HTML template file:", err)
	}
	fsTsVal.Store(ts)

	thisSrvr, err := NewSrvr(srvrIP, srvrPort, name)
	if err != nil {
		fatalerr(err)
	}
	srvrMap.Store(thisSrvr.AddrString(), thisSrvr)
	thisSrvrBytes = thisSrvr.ToBytes()

	srvrAddr := net.JoinHostPort(srvrIP, strconv.Itoa(srvrPort))
	http.HandleFunc("/", homeHandler)
	http.HandleFunc("/parse", parseHandler)
	http.HandleFunc("/favicon.ico", http.NotFound)
	http.Handle(
		"/fs/",
		http.StripPrefix("/fs", http.HandlerFunc(fsHandler)),
	)
	errChan := make(chan error)
	go func() {
    log.Println("Running server on:", srvrAddr)
		errChan <- http.ListenAndServe(srvrAddr, nil)
	}()
	select {
	case <-time.After(time.Second * 2):
	case err := <-errChan:
		fatalerr("error starting HTTP server:", err)
	}

	pc, err := net.ListenPacket(
		"udp4", net.JoinHostPort(udpIP, strconv.Itoa(udpPort)),
	)
	if err != nil {
		fatalerr("error starting server:", err)
	}
	broadcastAddr, err = net.ResolveUDPAddr(
		"udp4",
		net.JoinHostPort(broadcastIP, strconv.Itoa(broadcastPort)),
	)
	if err != nil {
		fatalerr("error starting server:", err)
	}

	go handleMsgs()
	go listen2(pc)
	go ping2(pc)
	go check2()

	log.Fatalln("error running HTTP server:", <-errChan)
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	ts := homeTsVal.Load().(*template.Template)
	var srvrs []*Srvr
	srvrMap.Range(func(_, iSrvr any) bool {
		srvrs = append(srvrs, iSrvr.(*Srvr))
		return true
	})
  sort.Slice(srvrs, func(i, j int) bool {
    return srvrs[i].Name < srvrs[j].Name
  })
	if err := ts.Execute(w, srvrs); err != nil {
		log.Println("error executing template:", err)
		//http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func fsHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("download") == "1" {
		downloadHandler(w, r)
		return
	}
	path := pathpkg.Join(fsDir, r.URL.Path)
	info, err := os.Stat(path)
	if err != nil {
		// NOTE: Just treat all errors as not found for right now
		if err == os.ErrNotExist || true {
			http.NotFound(w, r)
		} else {
			log.Println("error getting path info:", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}
	if info.IsDir() {
		serveDir(w, r, path)
		return
	}
	http.ServeFile(w, r, path)
}

func serveDir(w http.ResponseWriter, r *http.Request, path string) {
	ents, err := os.ReadDir(path)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		log.Println("error reading directory:", err)
		return
	}
	ts := fsTsVal.Load().(*template.Template)
	if err := ts.Execute(w, ents); err != nil {
		log.Println("error executing template:", err)
		//http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	path := pathpkg.Join(fsDir, r.URL.Path)
	info, err := os.Stat(path)
	if err != nil {
		// NOTE: Just treat all errors as not found for right now
		if err == os.ErrNotExist || true {
			http.NotFound(w, r)
		} else {
			log.Println("error getting path info:", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}
	if info.IsDir() {
		downloadDir(w, r, path)
		return
	}
	w.Header().Set(
		"Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"`, pathpkg.Base(path)),
	)
	http.ServeFile(w, r, path)
}

func downloadDir(w http.ResponseWriter, r *http.Request, path string) {
	filename := pathpkg.Base(path)
	if r.URL.Query().Get("tar") == "1" {
		w.Header().Set(
			"Content-Disposition",
			fmt.Sprintf(`attachment; filename="%s.tar.gz"`, filename),
		)
		downloadTar(w, r, path)
	} else {
		w.Header().Set(
			"Content-Disposition",
			fmt.Sprintf(`attachment; filename="%s.zip"`, filename),
		)
		downloadZip(w, r, path)
	}
}

func downloadZip(w http.ResponseWriter, r *http.Request, path string) {
	zw := zip.NewWriter(w)
	defer zw.Close()
	filepath.Walk(path, func(path string, info fs.FileInfo, err error) error {
		fmt.Println(path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := zw.Create(path)
		if err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(f, file)
		return err
	})
}

func downloadTar(w http.ResponseWriter, r *http.Request, path string) {
	gw := gzip.NewWriter(w)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()
	filepath.Walk(path, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, info.Name())
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(path)
		if info.IsDir() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = io.Copy(tw, file)
		return err
	})
}

func parseHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/") {
		path = path[1:]
	}
	var err error
	if path == "/parse/home" {
		err = parseHomeTemp()
	} else if path == "/parse/fs" {
		err = parseFsTemp()
	} else {
		err = parseHomeTemp()
		if e := parseFsTemp(); err == nil {
			err = e
		} else {
			err = fmt.Errorf("%v | %v", err, e)
		}
	}
	if err != nil {
		http.Error(
			w,
			fmt.Sprintf("error parsing template: %v", err),
			http.StatusBadRequest,
		)
		log.Println("error parsing template:", err)
		return
	}
	w.Write([]byte("Parsed"))
}

func parseHomeTemp() error {
	ts, err := template.ParseFiles(homeTempPath)
	if err != nil {
		return err
	}
	homeTsVal.Store(ts)
	return nil
}

func parseFsTemp() error {
	ts, err := template.ParseFiles(fsTempPath)
	if err != nil {
		return err
	}
	fsTsVal.Store(ts)
	return nil
}

func listen2(pc net.PacketConn) {
	var buf [128]byte
	for {
		n, addr, err := pc.ReadFrom(buf[:])
		if err != nil {
			log.Println("error reading:", err)
			continue
		}
		msgChan <- newMsgInfo(buf[:n], addr)
	}
}

func ping2(pc net.PacketConn) {
	for {
		time.Sleep(time.Millisecond * 4900)
		if _, err := pc.WriteTo(thisSrvrBytes, broadcastAddr); err != nil { // TODO
			log.Fatalln("error broadcasting:", err)
		}
	}
}

func check2() {
	const checkTime = time.Second * 15
	for {
		time.Sleep(checkTime)
		t := uint64(time.Now().Unix())
		srvrMap.Range(func(iAddr, iSrvr any) bool {
			if t > iSrvr.(*Srvr).GetLastPing()+uint64(checkTime) {
				srvrMap.Delete(iAddr)
			}
			return true
		})
	}
}

type MsgInfo struct {
	Data []byte
	Addr net.Addr
}

func newMsgInfo(data []byte, addr net.Addr) MsgInfo {
	return MsgInfo{Data: data, Addr: addr}
}

func handleMsgs() {
	for msgInfo := range msgChan {
		srvr := SrvrFromBytes(msgInfo.Data)
		iSrvr, _ := srvrMap.LoadOrStore(srvr.AddrString(), srvr)
		iSrvr.(*Srvr).SetLastPing(uint64(time.Now().Unix()))
	}
}

type Srvr struct {
	IP       [4]byte
	Port     uint16
	Name     string
	lastPing uint64
}

func NewSrvr(ip string, port int, name string) (*Srvr, error) {
	var ipBytes [4]byte
	start := 0
	for i := 0; i < 4; i++ {
		end := strings.IndexByte(ip[start:], '.')
		if end == -1 {
			if i != 3 {
				return nil, fmt.Errorf("invalid server IPv4 address")
			}
			end = len(ip)
		} else if i == 3 && end != -1 {
			return nil, fmt.Errorf("invalid server IPv4 address")
		} else {
			end += start
		}
		b, err := strconv.ParseUint(ip[start:end], 0, 8)
		if err != nil {
			return nil, fmt.Errorf("invalid server IPv4 address: %v", err)
		}
		ipBytes[i], start = byte(b), end+1
	}
	return &Srvr{
		IP:       ipBytes,
		Port:     uint16(port),
		Name:     name,
		lastPing: uint64(time.Now().Unix()),
	}, nil
}

func SrvrFromBytes(b []byte) *Srvr {
	return &Srvr{
		IP:   [4]byte{b[0], b[1], b[2], b[3]},
		Port: get2(b[4:]),
		Name: string(b[6:]),
	}
}

func (s *Srvr) ToBytes() []byte {
	return append(s.IP[:], append(put2(s.Port), s.Name...)...)
}

func (s *Srvr) AddrString() string {
	return fmt.Sprintf(
		"%d.%d.%d.%d:%d",
		s.IP[0], s.IP[1], s.IP[2], s.IP[3], s.Port,
	)
}

func (s *Srvr) AddrURLString() template.URL {
	return template.URL(s.AddrString())
}

func (s *Srvr) SetLastPing(t uint64) {
	atomic.StoreUint64(&s.lastPing, t)
}

func (s *Srvr) GetLastPing() uint64 {
	return atomic.LoadUint64(&s.lastPing)
}

type Options struct {
	BroadcastIP   string `json:"broadcast-ip,omitempty"`
	BroadcastPort int    `json:"broadcast-port,omitempty"`
	UdpIP         string `json:"udp-ip,omitempty"`
	UdpPort       int    `json:"udp-port,omitempty"`
	SrvrIP        string `json:"srvr-ip,omitempty"`
	SrvrPort      int    `json:"srvr-port,omitempty"`
	Name          string `json:"name,omitempty"`
	FsDir         string `json:"fs-dir,omitempty"`
	TempDir       string `json:"temp-dir,omitempty"`
}

func getOpts(path string) (*Options, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opts := &Options{}
	if err := json.NewDecoder(f).Decode(opts); err != nil {
		return nil, err
	}
	return opts, nil
}

func get2(b []byte) uint16 {
	return (uint16(b[0]) << 8) | uint16(b[1])
}

func put2(u uint16) []byte {
	return []byte{byte(u >> 8), byte(u)}
}

func fatalerr(args ...any) {
	fmt.Fprintln(os.Stderr, args...)
	os.Exit(1)
}
