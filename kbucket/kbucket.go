package kbucket

import (
	"errors"
	"net"
	"sort"
	"time"

	"github.com/kataras/golog"
)

type (
	notetype uint8
	MailType uint8
	note     struct {
		typ    notetype
		arg    interface{}
		result chan interface{}
	}
	Mail struct {
		Type   MailType
		Arg    []interface{}
		Result chan interface{}
	}
	KbConfig struct {
		Seeds   []string
		LocalIP net.IP
		Port    uint32
		ID      NodeID
	}
	Kbucket struct {
		config   *KbConfig
		routes   map[int]KQue
		Self     *Node
		store    *Storage
		k        int
		alpha    int
		ticker   *time.Ticker
		receiver chan note
		Sender   chan Mail
	}
)

const (
	nDelNode notetype = 0x01
	nAddNode notetype = 0x02
	nFindOne notetype = 0x03
	nFind    notetype = 0x04
	nStore   notetype = 0x05
)

const (
	MailPing MailType = 0x06
	MailFind MailType = 0x07
)
const (
	kcount = 8
	alpha  = 3
	ticktm = 5 * time.Second
)

//New create a kbucket
func New(config *KbConfig) *Kbucket {
	n := NewNode(config.ID, config.LocalIP, config.Port)
	k := &Kbucket{
		config:   config,
		routes:   make(map[int]KQue, 64),
		Self:     &n,
		store:    NewStorage(),
		k:        kcount,
		alpha:    alpha,
		receiver: make(chan note),
		Sender:   make(chan Mail),
	}
	k.ticker = time.NewTicker(ticktm)
	return k
}

func (k *Kbucket) Start() {
	go k.run()
}

func (k *Kbucket) run() {
	for {
		select {
		case <-k.ticker.C:
			golog.Info("[kbucket.run] routes: ", k.routes)
		case msg := <-k.receiver:
			switch msg.typ {
			case nAddNode:
				n := msg.arg.(Node)
				k.add(n)
			case nDelNode:
				n := msg.arg.(Node)
				k.remove(n)
			case nFind:
				nid := msg.arg.(NodeID)
				k.find(nid, msg.result)
			case nFindOne:
				nid := msg.arg.(NodeID)
				k.findOne(nid, msg.result)
			case nStore:
				kv := msg.arg.(struct {
					key   string
					value string
				})
				k.storeKV(kv.key, kv.value)
			}
		}
	}
}

//AddNode to add a node
func (k *Kbucket) AddNode(n Node) {
	k.receiver <- note{
		typ: nAddNode,
		arg: n,
	}
}
func (k *Kbucket) add(n Node) {
	distance, err := CalDistance(n.ID, k.Self.ID)
	if err != nil {
		golog.Error(err)
	}
	partion := distance.Partion()
	var que KQue
	if _, ok := k.routes[partion]; !ok {
		que = newKQue(k)
	} else {
		que = k.routes[partion]
	}
	qptr := &que
	qptr.updateAdd(n)
	k.routes[partion] = que
	return
}

//RemoveNode to remove a node
func (k *Kbucket) RemoveNode(n Node) {
	k.receiver <- note{
		typ: nDelNode,
		arg: n,
	}
}
func (k *Kbucket) remove(n Node) {
	distance, err := CalDistance(n.ID, k.Self.ID)
	if err != nil {
		golog.Error(err)
	}
	partion := distance.Partion()
	if _, ok := k.routes[partion]; !ok {
		return
	}
	que := k.routes[partion]
	que.remove(n)
	k.routes[partion] = que
	return
}
func (k *Kbucket) Find(nid NodeID) (ns []Node, err error) {
	phone := make(chan interface{})
	k.receiver <- note{
		typ:    nFind,
		arg:    nid,
		result: phone,
	}
	result, ok := <-phone
	if !ok {
		return ns, errors.New("Failed")
	}
	return result.([]Node), nil
}

//Find find alpha nodes that are closest to the nid
func (k *Kbucket) find(nid NodeID, phone chan interface{}) {
	defer close(phone)
	var ns []Node
	if k.Self.ID.Equal(nid) {
		return
	}
	dist, err := CalDistance(nid, k.Self.ID)
	if err != nil {
		golog.Error(err)
		return
	}
	partion := dist.Partion()
	if kq, ok := k.routes[partion]; ok {
		ns, err = kq.findN(nid, k.alpha)
		if err != nil {
			return
		}
	}
	if k.alpha <= len(ns) {
		return
	}
	pslice := make([]int, len(k.routes))
	i := 0
	for key := range k.routes {
		pslice[i] = key
		i++
	}
	p := Partions{
		parts: pslice,
		base:  partion,
	}
	sort.Sort(p)
	for _, v := range p.parts {
		kq, ok := k.routes[v]
		if ok {
			res, err := kq.findN(nid, k.alpha-len(ns))
			if err != nil {
				return
			}
			for _, v := range res {
				ns = append(ns, v)
			}
			if k.alpha <= len(ns) {
				break
			}
		}
	}
	phone <- ns
}

func (k *Kbucket) FindOne(nid NodeID) (Node, error) {
	phone := make(chan interface{})
	k.receiver <- note{
		typ:    nFindOne,
		arg:    nid,
		result: phone,
	}
	result, ok := <-phone
	if !ok {
		return Node{}, errors.New("Failed")
	}
	return result.(Node), nil
}

func (k *Kbucket) findOne(nid NodeID, phone chan interface{}) (Node, error) {
	if k.Self.ID.Equal(nid) {
		return *k.Self, nil
	}
	dist, err := CalDistance(nid, k.Self.ID)
	if err != nil {
		golog.Error(err)
		return Node{}, nil
	}
	partion := dist.Partion()
	kq, ok := k.routes[partion]
	if ok {
		ok, n := kq.findOne(nid)
		if ok {
			return n, nil
		}
	}

	pslice := make([]int, len(k.routes))
	i := 0
	for key := range k.routes {
		pslice[i] = key
		i++
	}
	p := Partions{
		parts: pslice,
		base:  partion,
	}
	sort.Sort(p)
	for _, v := range p.parts {
		kq, ok := k.routes[v]
		if ok {
			ok, n := kq.findOne(nid)
			if ok {
				return n, nil
			}
		}
	}
	return Node{}, errors.New("NOT FOUND")
}

func (k *Kbucket) Store(key, value string) {
	k.receiver <- note{
		typ: nStore,
		arg: struct {
			key   string
			value string
		}{key, value},
	}
}
func (k *Kbucket) storeKV(key, value string) {
	k.store.Put(key, value)
}

func (k *Kbucket) send(mt MailType, data []interface{}) (interface{}, error) {
	switch mt {
	case MailPing:
		mail := Mail{
			Type:   mt,
			Arg:    data,
			Result: make(chan interface{}),
		}
		k.Sender <- mail
		nid, ok := <-mail.Result
		if !ok {
			return nil, errors.New("Ping Failed")
		}
		return nid, nil
	case MailFind:
		mail := Mail{
			Type:   mt,
			Arg:    data,
			Result: make(chan interface{}),
		}
		k.Sender <- mail
		ns, ok := <-mail.Result
		if !ok {
			return nil, errors.New("Find Failed")
		}
		return ns, nil
	}
	return nil, nil
}
