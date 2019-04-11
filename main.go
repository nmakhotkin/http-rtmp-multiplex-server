package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/jkuri/http-rtmp-multiplex-server/rtmp"
	"github.com/nareix/joy4/av/avutil"
	"github.com/nareix/joy4/av/pubsub"
	"github.com/nareix/joy4/format"
	"github.com/nareix/joy4/format/flv"
	"github.com/soheilhy/cmux"
)

var (
	port int
	logger = log.New(os.Stdout, "http: ", log.LstdFlags)
)


func init() {
	format.RegisterAll()
}

type writeFlusher struct {
	httpFlusher http.Flusher
	io.Writer
}

func (wf writeFlusher) Flush() error {
	wf.httpFlusher.Flush()
	return nil
}

func main() {
	flag.IntVar(&port, "port", 1935, "TCP port to run on")
	flag.Parse()

	server := &rtmp.Server{}
	l := &sync.RWMutex{}

	type Channel struct {
		que *pubsub.Queue
	}
	channels := map[string]*Channel{}

	server.HandlePlay = func(conn *rtmp.Conn) {
		defer conn.Close()

		l.RLock()
		ch := channels[conn.URL.Path]
		l.RUnlock()

		log.Println("Try playing", conn.URL.Path)
		if ch != nil {
			if err := avutil.CopyFile(conn, ch.que.Latest()); err != nil && err != io.EOF {
				log.Println("Unable to serve stream:", err)
			}
		} else {
			// Keep connect for 2 sec and close
			log.Println("No such channel yet:", conn.URL.Path)
			log.Println("Waiting for publishing channel", conn.URL.Path)
			ticker := time.NewTicker(time.Millisecond * 500)
			for range ticker.C {
				l.RLock()
				ch = channels[conn.URL.Path]
				l.RUnlock()
				if ch != nil {
					break
				}
			}
			ticker.Stop()
			time.Sleep(time.Millisecond * 1500)
			if err := avutil.CopyFile(conn, ch.que.Latest()); err != nil && err != io.EOF {
				log.Println("Unable to serve stream:", err)
			}

		}
	}

	server.HandlePublish = func(conn *rtmp.Conn) {
		defer conn.Close()

		streams, err := conn.Streams()
		if err != nil {
			log.Println("Unable to stream:", err)
			return
		}

		l.Lock()
		ch := channels[conn.URL.Path]
		if ch == nil {
			ch = &Channel{}
			ch.que = pubsub.NewQueue()
			_ = ch.que.WriteHeader(streams)
			channels[conn.URL.Path] = ch
		} else {
			ch = nil
		}
		l.Unlock()
		if ch == nil {
			return
		}
		log.Println("Started streaming", conn.URL.Path)

		if err := avutil.CopyPackets(ch.que, conn); err == io.EOF {
			log.Println("Stopped streaming", conn.URL.Path)
		} else if err != nil {
			log.Printf("Unable to stream %v: %v", conn.URL.Path, err)
		}

		l.Lock()
		delete(channels, conn.URL.Path)
		l.Unlock()
		ch.que.Close()
	}

	httpFlvHandler := func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			l.RLock()
			ch := channels[r.URL.Path]
			l.RUnlock()

			if ch != nil {
				w.Header().Set("Content-Type", "video/x-flv")
				w.Header().Set("Transfer-Encoding", "chunked")
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.WriteHeader(200)
				flusher := w.(http.Flusher)
				flusher.Flush()

				muxer := flv.NewMuxerWriteFlusher(writeFlusher{httpFlusher: flusher, Writer: w})
				cursor := ch.que.Latest()

				avutil.CopyFile(muxer, cursor)
			} else {
				http.NotFound(w, r)
			}
		})
	}

	router := http.NewServeMux()
	router.Handle("/", httpFlvHandler())

	ln, err := net.Listen("tcp", fmt.Sprintf(":%v", port))
	if err != nil {
		log.Fatal(err)
	}

	m := cmux.New(ln)

	httpL := m.Match(cmux.HTTP1Fast())
	rtmpL := m.Match(cmux.Any())

	log.Printf("Server is starting at ::%v...\n", port)

	httpServer := &http.Server{
		Handler: logging(logger)(router),
	}

	go func() {
		if err := httpServer.Serve(httpL); err != nil {
			log.Println("Error serve http:", err)
		}
	}()
	go func() {
		if err := server.ListenAndServe(rtmpL); err != nil {
			log.Println("Error serve rtmp:", err)
		}
	}()

	if err := m.Serve(); err != nil {
		log.Println("Error serve:", err)
	}
}

func logging(log *log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				log.Println(r.Method, r.URL.Path, r.RemoteAddr, r.UserAgent())
			}()
			next.ServeHTTP(w, r)
		})
	}
}
