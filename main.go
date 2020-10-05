package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mafredri/cdp"
	"github.com/mafredri/cdp/protocol/emulation"
	"github.com/mafredri/cdp/rpcc"

	socketio "github.com/googollee/go-socket.io"
	"github.com/googollee/go-socket.io/engineio"
	"github.com/googollee/go-socket.io/engineio/transport"
	"github.com/googollee/go-socket.io/engineio/transport/polling"
)

const base = "/caster"

const uri = "https://google.com"

//const uri = "http://localhost:5000"

type Client struct {
	id   string
	sock socketio.Conn
	port int
	cdp  *cdp.Client
	proc *os.Process
	data chan []byte
	stop chan struct{}
}

var clients = map[string]*Client{}

type Event struct {
	Type       string  `json:"type"`
	X          int     `json:"x"`
	Y          int     `json:"y"`
	ClickCount int     `json:"clickCount"`
	Button     string  `json:"button"`
	DeltaY     float32 `json:"deltaY"`
	DeltaX     float32 `json:"deltaX"`

	Key       string `json:"key"`
	KeyCode   int    `json:"keyCode"`
	Code      string `json:"code"`
	Modifiers int    `json:"modifiers"`
	Text      string `json:"text"`

	Width  int `json:"width"`
	Height int `json:"height"`

	Url string `json:"url"`
}

func main() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigs
		log.Printf("got signal %v", sig)
		for _, client := range clients {
			select {
			case client.stop <- struct{}{}:
			default:
			}
		}
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}()

	http.Handle(base+"/", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != base+"/" {
			rw.WriteHeader(404)
			return
		}
		data, err := ioutil.ReadFile("./tpl.html")
		if err != nil {
			rw.WriteHeader(500)
			rw.Write([]byte(fmt.Sprintf("%v", err)))
			return
		}
		rw.Header().Add("Content-Type", "text/html")
		rw.Write(data)
	}))

	sioserver := sio()
	defer sioserver.Close()
	http.Handle(base+"/sock/", sioserver)

	panic(http.ListenAndServe("localhost:5555", nil))
}

func sio() (server *socketio.Server) {
	server, _ = socketio.NewServer(&engineio.Options{
		Transports: []transport.Transport{
			polling.Default,
		},
	})
	server.OnConnect("/", func(s socketio.Conn) error {
		s.SetContext("")
		log.Println("connected:", s.ID())
		return nil
	})

	server.OnEvent("/", "start", func(s socketio.Conn, evt Event) {
		start(s, evt)
	})

	server.OnEvent("/", "event", func(s socketio.Conn, msg string) {
		log.Printf("event: %s", msg)
	})

	server.OnError("/", func(s socketio.Conn, e error) {
		log.Println("error:", e)
	})

	server.OnDisconnect("/", func(s socketio.Conn, reason string) {
		if client, found := clients[s.ID()]; found {
			client.stop <- struct{}{}
		}
		log.Println("closed", reason)
	})

	go server.Serve()

	return
}

func start(sock socketio.Conn, evt Event) (err error) {
	client, found := clients[sock.ID()]
	if !found {
		client = &Client{
			id:   sock.ID(),
			sock: sock,
			stop: make(chan struct{}),
			data: make(chan []byte, 10),
		}
		clients[sock.ID()] = client
	}
	if client.proc != nil {
		log.Printf("killing %d on %d", client.proc.Pid, client.port)
		if err = client.proc.Kill(); err != nil {
			err = fmt.Errorf("clould not kill %d: %w", client.proc.Pid, err)
			return
		}
	}
	client.port, client.proc, err = startChrome(evt.Width, evt.Height)
	if err != nil {
		return
	}
	go func(client *Client) {
		var err error
		var conn *rpcc.Conn
		ctx, cancel := context.WithCancel(context.TODO())
		defer cancel()
		if client.cdp, conn, err = newClient(ctx, client.port); err != nil {
			log.Printf("ERROR newClient err %v", err)
			sock.Emit("error", fmt.Sprintf("newClient error %v", err))
			return
		}
		time.Sleep(100 * time.Millisecond)
		defer conn.Close()
		go func(client *Client) {
			for frame := range client.data {
				client.sock.Emit("frame", frame)
			}
		}(client)
		defer close(client.data)
		if err := client.cdp.Emulation.SetDeviceMetricsOverride(context.TODO(), emulation.NewSetDeviceMetricsOverrideArgs(evt.Width, evt.Height, 1, false)); err != nil {
			log.Printf("resize err %v", err)
			sock.Emit("error", fmt.Sprintf("set size error %v", err))
		}
		if err := screencast(ctx, client.port, client.cdp, evt.Width, evt.Height, client.data, client.stop); err != nil {
			log.Printf("ERROR run err %v", err)
			sock.Emit("error", fmt.Sprintf("start screencast error %v", err))
		}
	}(client)
	return
}

/*func ws(w http.ResponseWriter, r *http.Request) {
	var c *cdp.Client
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}
	client := make(chan []byte, 100)
	stop := make(chan struct{}, 1)
	lock.Lock()
	stops[idx] = stop
	clientid := idx
	idx += 1
	lock.Unlock()
	defer conn.Close()
	go func() {
		var proc *os.Process
		var port int
		for {
			evt := event{}
			t, data, err := conn.ReadMessage()
			if err != nil {
				log.Printf("got err %v", err)
				break
			}
			log.Printf("got type %d, data %s,", t, string(data))
			if t == websocket.CloseMessage {
				break
			}
			if err := json.Unmarshal(data, &evt); err != nil {
				log.Printf("got marshal err %v", err)
				continue
			}
			if evt.Type == "start" {
				if proc != nil {
					log.Printf("killing %d on %d", proc.Pid, port)
					if err := proc.Kill(); err != nil {
						log.Printf("clould not kill %d: %v", proc.Pid, err)
					}
				}
				port, proc, err = startChrome(evt.Width, evt.Height)
				if err != nil {
					w.WriteHeader(500)
					w.Write([]byte(fmt.Sprintf("start chrome err %v", err)))
					break
				}
				go func() {
					var err error
					var conn *rpcc.Conn
					ctx, cancel := context.WithCancel(context.TODO())
					defer cancel()
					if c, conn, err = newClient(ctx, port); err != nil {
						log.Printf("ERROR newClient err %v", err)
						w.WriteHeader(500)
						return
					}
					time.Sleep(100 * time.Millisecond)
					defer conn.Close()
					if err := c.Emulation.SetDeviceMetricsOverride(context.TODO(), emulation.NewSetDeviceMetricsOverrideArgs(evt.Width, evt.Height, 1, false)); err != nil {
						log.Printf("resize err %v", err)
					}
					if err := run(ctx, port, c, evt.Width, evt.Height, client, stop); err != nil {
						log.Printf("ERROR run err %v", err)
						w.WriteHeader(500)
					}
				}()
				continue
			} else if evt.Type == "resize" {
				if err := c.Emulation.SetDeviceMetricsOverride(context.TODO(), emulation.NewSetDeviceMetricsOverrideArgs(evt.Width, evt.Height, 1, false)); err != nil {
					log.Printf("resize err %v", err)
				}
			}
			if c == nil {
				continue
			}
			if strings.HasPrefix(evt.Type, "navigate") {
				switch evt.Type {
				case "navigateTo":
					repl, err := c.Page.Navigate(context.TODO(), page.NewNavigateArgs(evt.Url))
					if err != nil {
						log.Printf("navigate error %v", err)
						continue
					}
					if repl.ErrorText != nil {
						log.Printf("naviagate reply error %s", *repl.ErrorText)
					}
				case "navigateBack", "navigateForward":
					his, err := c.Page.GetNavigationHistory(context.TODO())
					if err != nil {
						log.Printf("get history error %v", err)
						continue
					}
					idx := his.CurrentIndex
					if evt.Type == "navigateBack" && idx > 0 {
						idx--
					} else if evt.Type == "navigateForward" && idx < len(his.Entries)-1 {
						idx++
					}
					if idx == his.CurrentIndex {
						continue
					}
					if err := c.Page.NavigateToHistoryEntry(context.TODO(), page.NewNavigateToHistoryEntryArgs(his.Entries[idx].ID)); err != nil {
						log.Printf("navigateback error %v", err)
					}
				}
			} else if strings.HasPrefix(evt.Type, "mouse") {
				log.Printf("got mouse event %s", evt.Type)
				args := input.NewDispatchMouseEventArgs(evt.Type, float64(evt.X), float64(evt.Y))
				args.SetModifiers(evt.Modifiers).SetButton(input.MouseButton(evt.Button))
				if evt.ClickCount > 0 {
					args.SetClickCount(evt.ClickCount)
				}
				if evt.Type == "mouseWheel" {
					args.SetDeltaX(float64(evt.DeltaX)).SetDeltaY(float64(evt.DeltaY))
				}
				if err := c.Input.DispatchMouseEvent(context.TODO(), args); err != nil {
					log.Printf("mouse err %v", err)
				}
			} else if strings.HasPrefix(evt.Type, "key") || evt.Type == "char" {
				args := input.NewDispatchKeyEventArgs(evt.Type)
				args.SetCode(evt.Code).SetKey(evt.Key).SetModifiers(evt.Modifiers).SetWindowsVirtualKeyCode(evt.KeyCode).SetNativeVirtualKeyCode(evt.KeyCode)
				if evt.Text != "" {
					args.SetText(evt.Text)
				}
				if err := c.Input.DispatchKeyEvent(context.TODO(), args); err != nil {
					log.Printf("key err %v", err)
				}
			}
		}
		if stop != nil {
			stop <- struct{}{}
		}
		if proc != nil {
			log.Printf("killing %d on %d", proc.Pid, port)
			if err := proc.Kill(); err != nil {
				log.Printf("clould not kill %d: %v", proc.Pid, err)
			}
		}
	}()

	for data := range client {
		if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
			log.Printf("error: could not write message %v", err)
			break
		}
	}
	delete(stops, clientid)
}
*/
