package main

import (
	// _ "net/http/pprof"
	"os"
	"os/signal"
	"time"

	"github.com/subgraph/fw-daemon/Godeps/_workspace/src/github.com/op/go-logging"
	"github.com/subgraph/fw-daemon/nfqueue"
	"github.com/subgraph/fw-daemon/proc"
	"sync"
	"syscall"
	"unsafe"
)

var log = logging.MustGetLogger("sgfw")

var logFormat = logging.MustStringFormatter(
	"%{level:.4s} %{id:03x} %{message}",
)
var ttyFormat = logging.MustStringFormatter(
	"%{color}%{time:15:04:05} ▶ %{level:.4s} %{id:03x}%{color:reset} %{message}",
)

const ioctlReadTermios = 0x5401

func isTerminal(fd int) bool {
	var termios syscall.Termios
	_, _, err := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), ioctlReadTermios, uintptr(unsafe.Pointer(&termios)), 0, 0, 0)
	return err == 0
}

func setupLoggerBackend() logging.LeveledBackend {
	format := logFormat
	if isTerminal(int(os.Stderr.Fd())) {
		format = ttyFormat
	}
	backend := logging.NewLogBackend(os.Stderr, "", 0)
	formatter := logging.NewBackendFormatter(backend, format)
	leveler := logging.AddModuleLevel(formatter)
	return leveler
}

var logRedact bool

type Firewall struct {
	dbus *dbusServer
	dns  *dnsCache

	enabled bool

	logBackend logging.LeveledBackend

	lock      sync.Mutex
	policyMap map[string]*Policy
	policies  []*Policy

	ruleLock   sync.Mutex
	rulesById  map[uint]*Rule
	nextRuleId uint
}

func (fw *Firewall) setEnabled(flag bool) {
	fw.lock.Lock()
	defer fw.lock.Unlock()
	fw.enabled = flag
}

func (fw *Firewall) isEnabled() bool {
	fw.lock.Lock()
	defer fw.lock.Unlock()
	return fw.enabled
}

func (fw *Firewall) clearRules() {
	fw.ruleLock.Lock()
	defer fw.ruleLock.Unlock()
	fw.rulesById = nil
	fw.nextRuleId = 0
}

func (fw *Firewall) addRule(r *Rule) {
	fw.ruleLock.Lock()
	defer fw.ruleLock.Unlock()

	r.id = fw.nextRuleId
	fw.nextRuleId += 1
	if fw.rulesById == nil {
		fw.rulesById = make(map[uint]*Rule)
	}
	fw.rulesById[r.id] = r
}

func (fw *Firewall) getRuleById(id uint) *Rule {
	fw.ruleLock.Lock()
	defer fw.ruleLock.Unlock()

	if fw.rulesById == nil {
		return nil
	}
	return fw.rulesById[id]
}

func (fw *Firewall) runFilter() {
	q := nfqueue.NewNFQueue(0)
	defer q.Destroy()

	q.DefaultVerdict = nfqueue.DROP
	q.Timeout = 5 * time.Minute
	packets := q.Process()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, os.Kill)

	for {
		select {
		case pkt := <-packets:
			if fw.isEnabled() {
				fw.filterPacket(pkt)
			} else {
				pkt.Accept()
			}
		case <-sigs:
			return
		}
	}
}

func main() {
	logBackend := setupLoggerBackend()
	log.SetBackend(logBackend)
	proc.SetLogger(log)

	if os.Geteuid() != 0 {
		log.Error("Must be run as root")
		os.Exit(1)
	}

	setupIPTables()

	ds, err := newDbusServer()
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}

	fw := &Firewall{
		dbus:       ds,
		dns:        NewDnsCache(),
		enabled:    true,
		logBackend: logBackend,
		policyMap:  make(map[string]*Policy),
	}
	ds.fw = fw

	fw.loadRules()

	/*
		go func() {
			http.ListenAndServe("localhost:6060", nil)
		}()
	*/

	fw.runFilter()
}
