package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mafredri/cdp"
	"github.com/mafredri/cdp/devtool"
	"github.com/mafredri/cdp/protocol/page"
	"github.com/mafredri/cdp/rpcc"
)

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

func screencast(ctx context.Context, port int, c *cdp.Client, w, h int, client chan []byte, stop chan struct{}) error {
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
