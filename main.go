package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mafredri/cdp"
	"github.com/mafredri/cdp/devtool"
	"github.com/mafredri/cdp/protocol/emulation"
	"github.com/mafredri/cdp/protocol/input"
	"github.com/mafredri/cdp/protocol/page"
	"github.com/mafredri/cdp/rpcc"

	"github.com/gorilla/websocket"
)

const base = "/caster"

const uri = "https://google.com"

//const uri = "http://localhost:5000"

var upgrader = websocket.Upgrader{EnableCompression: true, CheckOrigin: func(r *http.Request) bool { return true }}

var stops = map[int]chan struct{}{}
var idx = 0
var lock sync.Mutex

type event struct {
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
		for _, s := range stops {
			select {
			case s <- struct{}{}:
			default:
			}
		}
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}()

	http.Handle(base+"/ws", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, h := range r.Header {
			log.Printf("%s: %s", k, strings.Join(h, ", "))
		}
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

	panic(http.ListenAndServe("localhost:5555", nil))
}

func newClient(ctx context.Context, port int) (c *cdp.Client, conn *rpcc.Conn, err error) {
	devt := devtool.New(fmt.Sprintf("http://localhost:%d", port))
	pageTarget, err := devt.Get(ctx, devtool.Page)
	if err != nil {
		return
	}

	for i := 0; i < 30; i++ {
		conn, err = rpcc.DialContext(ctx, pageTarget.WebSocketDebuggerURL)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
		log.Printf("try %d", i)
	}
	if err != nil {
		return
	}

	c = cdp.NewClient(conn)
	return
}

func run(ctx context.Context, port int, c *cdp.Client, w, h int, client chan []byte, stop chan struct{}) error {
	var err error
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

		for {
			ev, err := screencastFrame.Recv()
			if err != nil {
				log.Printf("Failed to receive ScreencastFrame: %v", err)
				return
			}

			go func() {
				err = c.Page.ScreencastFrameAck(ctx, page.NewScreencastFrameAckArgs(ev.SessionID))
				if err != nil {
					log.Printf("Failed to ack ScreencastFrame: %v", err)
					return
				}
			}()

			select {
			case client <- ev.Data:
			default:
			}
		}
	}()

	log.Printf("starting screencast on %d", port)
	screencastArgs := page.NewStartScreencastArgs().
		SetEveryNthFrame(2).
		SetFormat("jpeg").
		SetQuality(90).SetMaxHeight(2000).SetMaxWidth(2000)
	err = c.Page.StartScreencast(ctx, screencastArgs)
	if err != nil {
		return err
	}

	<-stop

	log.Printf("stopping screencast on %d", port)
	err = c.Page.StopScreencast(ctx)
	if err != nil {
		return err
	}

	return nil
}

func startChrome(width, height int) (port int, proc *os.Process, err error) {
	port = getFreePort()
	args := "--headless " + fmt.Sprintf("--remote-debugging-port=%d", port) + " " + fmt.Sprintf("--window-size=%d,%d", width, height)
	log.Println("args: ", args)
	cmd := exec.Command("./Google Chrome", strings.Split(args, " ")...)
	cmd.Dir = "/Applications/Google Chrome.app/Contents/MacOS"
	if err = cmd.Start(); err != nil {
		return
	}
	proc = cmd.Process
	time.Sleep(1000 * time.Millisecond)
	log.Printf("procID for %d: %d", port, cmd.Process.Pid)
	return
}

func getFreePort() int {
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		panic(err)
	}

	return lis.Addr().(*net.TCPAddr).Port
}
