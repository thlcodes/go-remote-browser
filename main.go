package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mafredri/cdp"
	"github.com/mafredri/cdp/devtool"
	"github.com/mafredri/cdp/protocol/input"
	"github.com/mafredri/cdp/protocol/page"
	"github.com/mafredri/cdp/rpcc"

	"github.com/gorilla/websocket"
)

const base = "/caster"

const uri = "https://google.com"

//const uri = "http://localhost:5000"

var upgrader = websocket.Upgrader{EnableCompression: true}

var clients = map[int]chan []byte{}
var idx = 0
var lock sync.Mutex
var stop chan struct{} = make(chan struct{}, 1)

var c *cdp.Client
var ctx context.Context

type event struct {
	Type    string `json:"type"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	Key     string `json:"key"`
	KeyCode int    `json:"keyCode`
	Code    string `json:"code`
}

func main() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigs
		log.Printf("got signal %v", sig)
		stop <- struct{}{}
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}()

	go func() {
		if err := run(); err != nil {
			stop <- struct{}{}
			panic(err)
		}
	}()

	http.Handle(base+"/ws", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Print("upgrade:", err)
			return
		}
		client := make(chan []byte, 100)
		lock.Lock()
		clients[idx] = client
		clientid := idx
		idx += 1
		lock.Unlock()
		defer conn.Close()
		go func() {
			for {
				evt := event{}
				t, data, err := conn.ReadMessage()
				if err != nil {
					log.Printf("got err %v", err)
					return
				}
				log.Printf("got type %d, data %s,", t, string(data))
				if t == websocket.CloseMessage {
					break
				}
				if err := json.Unmarshal(data, &evt); err != nil {
					log.Printf("got marshal err %v", err)
					continue
				}
				if c == nil {
					continue
				}
				if strings.HasPrefix(evt.Type, "mouse") {
					log.Printf("got mouse event %s", evt.Type)
					if err := c.Input.DispatchMouseEvent(ctx, input.NewDispatchMouseEventArgs(evt.Type, float64(evt.X), float64(evt.Y))); err != nil {
						log.Printf("mouse err %v", err)
					}
				} else if strings.HasPrefix(evt.Type, "key") {
					args := input.NewDispatchKeyEventArgs(evt.Type)
					args.SetCode(evt.Code).SetText(evt.Key)
					if err := c.Input.DispatchKeyEvent(ctx, args); err != nil {
						log.Printf("key err %v", err)
					}
				}

			}
		}()

		for data := range client {
			if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
				log.Printf("error: could not write message %v", err)
				break
			}
		}
		delete(clients, clientid)
	}))

	http.Handle(base+"/", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		data, err := ioutil.ReadFile("./tpl.html")
		if err != nil {
			rw.WriteHeader(500)
			rw.Write([]byte(fmt.Sprintf("%v", err)))
			return
		}
		rw.Header().Add("Content-Type", "text/html")
		rw.Write(data)
	}))

	http.ListenAndServe("localhost:5555", nil)
}

func run() error {
	var cancel func()
	ctx, cancel = context.WithCancel(context.TODO())
	defer cancel()

	devt := devtool.New("http://localhost:9222")

	pageTarget, err := devt.Get(ctx, devtool.Page)
	if err != nil {
		return err
	}

	conn, err := rpcc.DialContext(ctx, pageTarget.WebSocketDebuggerURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	c = cdp.NewClient(conn)

	err = c.Page.Enable(ctx)
	if err != nil {
		return err
	}

	// Navigate to GitHub, block until ready.
	loadEventFired, err := c.Page.LoadEventFired(ctx)
	if err != nil {
		return err
	}

	_, err = c.Page.Navigate(ctx, page.NewNavigateArgs(uri))
	if err != nil {
		return err
	}

	_, err = loadEventFired.Recv()
	if err != nil {
		return err
	}
	loadEventFired.Close()

	// Start listening to ScreencastFrame events.
	screencastFrame, err := c.Page.ScreencastFrame(ctx)
	if err != nil {
		return err
	}

	go func() {
		defer screencastFrame.Close()

		ts := time.Now()

		for {
			ev, err := screencastFrame.Recv()
			if err != nil {
				log.Printf("Failed to receive ScreencastFrame: %v", err)
				return
			}
			//log.Printf("Got frame with sessionID: %d: %+v", ev.SessionID, ev.Metadata)

			go func() {
				err = c.Page.ScreencastFrameAck(ctx, page.NewScreencastFrameAckArgs(ev.SessionID))
				if err != nil {
					log.Printf("Failed to ack ScreencastFrame: %v", err)
					return
				}
			}()

			took := time.Since(ts)
			txt := fmt.Sprintf("took: %d, fps: %d, size: %d", took.Milliseconds(), 1000/took.Milliseconds(), len(ev.Data))
			//log.Println(txt)
			go func(txt string) {
				lock.Lock()
				defer lock.Unlock()
				for _, client := range clients {
					//log.Printf("sending to client %d", i)
					select {
					case client <- []byte(ev.Data):
					default:
					}
				}
			}(txt)
			ts = time.Now()

			/*rand.Seed(time.Now().UnixNano())
			if err := c.Input.DispatchMouseEvent(ctx, input.NewDispatchMouseEventArgs("mouseMoved", rand.Float64()*1280, rand.Float64()*720)); err != nil {
				log.Printf("mousemove err %v", err)
			}*/
		}
	}()

	log.Println("starting screencast")
	screencastArgs := page.NewStartScreencastArgs().
		SetEveryNthFrame(2).
		SetFormat("png").
		SetQuality(100).
		SetMaxWidth(1280).SetMaxHeight(720)
	err = c.Page.StartScreencast(ctx, screencastArgs)
	if err != nil {
		return err
	}

	<-stop

	for _, client := range clients {
		if client != nil {
			close(client)
		}
	}

	log.Println("stopping screencast")
	err = c.Page.StopScreencast(ctx)
	if err != nil {
		return err
	}

	return nil
}
