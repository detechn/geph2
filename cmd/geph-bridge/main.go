package main

import (
	"bytes"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	statsd "github.com/etsy/statsd/examples/go"
	"github.com/geph-official/geph2/libs/bdclient"
	"github.com/geph-official/geph2/libs/cshirt2"
	"github.com/geph-official/geph2/libs/kcp-go"
	"github.com/geph-official/geph2/libs/niaucchi4"
	"golang.org/x/time/rate"
)

var cookieSeed string
var cookie []byte

var binderFront string
var binderReal string
var exitRegex string
var binderKey string
var statsdAddr string
var allocGroup string
var speedLimit int
var bclient *bdclient.Client

var limiter *rate.Limiter

var statClient *statsd.StatsdClient

func main() {
	flag.StringVar(&binderFront, "binderFront", "https://ajax.aspnetcdn.com/v2", "binder domain-fronting host")
	flag.StringVar(&binderReal, "binderReal", "gephbinder.azureedge.net", "real hostname of the binder")
	flag.StringVar(&exitRegex, "exitRegex", `\.exits\.geph\.io$`, "domain suffix for exit nodes")
	flag.StringVar(&statsdAddr, "statsdAddr", "c2.geph.io:8125", "address of StatsD for gathering statistics")
	flag.StringVar(&binderKey, "binderKey", "", "binder API key")
	flag.StringVar(&allocGroup, "allocGroup", "", "allocation group")
	flag.IntVar(&speedLimit, "speedLimit", 20000, "speed limit in KB/s")
	flag.Parse()
	limiter = rate.NewLimiter(rate.Limit(speedLimit*1024), 100*1000)
	if allocGroup == "" {
		log.Fatal("must specify an allocation group")
	}
	if statsdAddr != "" {
		z, e := net.ResolveUDPAddr("udp", statsdAddr)
		if e != nil {
			panic(e)
		}
		statClient = statsd.New(z.IP.String(), z.Port)
	}
	generateCookie()
	bclient = bdclient.NewClient(binderFront, binderReal)
	go func() {
		lastTotal := uint64(0)
		lastRetrans := uint64(0)
		for {
			time.Sleep(time.Second * 10)
			s := kcp.DefaultSnmp.Copy()
			deltaTotal := float64(s.OutSegs - lastTotal)
			lastTotal = s.OutSegs
			deltaRetrans := float64(s.RetransSegs - lastRetrans)
			lastRetrans = s.RetransSegs
			RL := int64(10000 * deltaRetrans / (deltaRetrans + deltaTotal))
			statClient.Timing(allocGroup+".lossPct", RL)
		}
	}()
	listenLoop()
}

func generateCookie() {
	cookie = make([]byte, 32)
	rand.Read(cookie)
}

func listenLoop() {
	udpsock, err := net.ListenPacket("udp", ":")
	if err != nil {
		panic(err)
	}
	udpsock.(*net.UDPConn).SetWriteBuffer(1000 * 1000 * 10)
	go func() {
		for {
			myAddr := fmt.Sprintf("%v:%v", guessIP(), udpsock.LocalAddr().(*net.UDPAddr).Port)
			log.Println("server started UDP on", myAddr)
			e := bclient.AddBridge(binderKey, cookie, myAddr)
			if e != nil {
				log.Println("error adding bridge:", e)
			}
			time.Sleep(time.Minute)
		}
	}()
	go func() {
		listener, err := net.Listen("tcp", udpsock.LocalAddr().String())
		if err != nil {
			panic(err)
		}
		log.Println("N4/TCP listener spinned up")
		for {
			rawClient, err := listener.Accept()
			if err != nil {
				panic(err)
			}
			go func() {
				defer rawClient.Close()
				rawClient.SetDeadline(time.Now().Add(time.Second * 10))
				client, err := cshirt2.Server(cookie, rawClient)
				rawClient.SetDeadline(time.Now().Add(time.Hour * 24))
				if err != nil {
					log.Println("cshirt2 failed", err, rawClient.RemoteAddr())
					return
				}
				log.Println("Accepted TCP from", rawClient.RemoteAddr())
				handle(client)
			}()
		}
	}()
	e2e := niaucchi4.ObfsListen(cookie, udpsock)
	if err != nil {
		panic(err)
	}
	listener := niaucchi4.ListenKCP(e2e)
	log.Println("N4/UDP listener spinned up")
	for {
		client, err := listener.Accept()
		if err != nil {
			panic(err)
		}
		log.Println("Accepted UDP client", client.RemoteAddr())
		go handle(client)
	}
}

func guessIP() string {
	resp, err := http.Get("https://checkip.amazonaws.com")
	if err != nil {
		panic("stuck while getting our own IP: " + err.Error())
	}
	buf := new(bytes.Buffer)
	io.Copy(buf, resp.Body)
	resp.Body.Close()
	myip := strings.Trim(string(buf.Bytes()), "\n ")
	return myip
}
