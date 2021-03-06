package scraperService

import (
	"github.com/ewhal/nyaa/config"
	"github.com/ewhal/nyaa/db"
	"github.com/ewhal/nyaa/model"
	"github.com/ewhal/nyaa/util/log"
	"net"
	"net/url"
	"time"
)

// MTU yes this is the ipv6 mtu
const MTU = 1500

// bittorrent scraper
type Scraper struct {
	done      chan int
	sendQueue chan *SendEvent
	recvQueue chan *RecvEvent
	errQueue  chan error
	trackers  map[string]*Bucket
	ticker    *time.Ticker
	interval  time.Duration
}

func New(conf *config.ScraperConfig) (sc *Scraper, err error) {
	sc = &Scraper{
		done:      make(chan int),
		sendQueue: make(chan *SendEvent, 1024),
		recvQueue: make(chan *RecvEvent, 1024),
		errQueue:  make(chan error),
		trackers:  make(map[string]*Bucket),
		ticker:    time.NewTicker(time.Second),
		interval:  time.Second * time.Duration(conf.IntervalSeconds),
	}
	for idx := range conf.Trackers {
		err = sc.AddTracker(&conf.Trackers[idx])
		if err != nil {
			break
		}
	}
	return
}

func (sc *Scraper) AddTracker(conf *config.ScrapeConfig) (err error) {
	var u *url.URL
	u, err = url.Parse(conf.URL)
	if err == nil {
		var ips []net.IP
		ips, err = net.LookupIP(u.Hostname())
		if err == nil {
			// TODO: use more than 1 ip ?
			addr := &net.UDPAddr{
				IP: ips[0],
			}
			addr.Port, err = net.LookupPort("udp", u.Port())
			if err == nil {
				sc.trackers[addr.String()] = NewBucket(addr)
			}
		}
	}
	return
}

func (sc *Scraper) Close() (err error) {
	close(sc.sendQueue)
	close(sc.recvQueue)
	close(sc.errQueue)
	sc.ticker.Stop()
	sc.done <- 1
	return
}

func (sc *Scraper) runRecv(pc net.PacketConn) {
	for {
		var buff [MTU]byte
		n, from, err := pc.ReadFrom(buff[:])

		if err == nil {

			log.Debugf("got %d from %s", n, from)
			sc.recvQueue <- &RecvEvent{
				From: from,
				Data: buff[:n],
			}
		} else {
			sc.errQueue <- err
		}
	}
}

func (sc *Scraper) runSend(pc net.PacketConn) {
	for {
		ev, ok := <-sc.sendQueue
		if !ok {
			return
		}
		log.Debugf("write %d to %s", len(ev.Data), ev.To)
		pc.WriteTo(ev.Data, ev.To)
	}
}

func (sc *Scraper) RunWorker(pc net.PacketConn) (err error) {

	go sc.runRecv(pc)
	go sc.runSend(pc)
	for {
		var bucket *Bucket
		ev, ok := <-sc.recvQueue
		if !ok {
			break
		}
		tid, err := ev.TID()
		action, err := ev.Action()
		log.Debugf("transaction = %d action = %d", tid, action)
		if err == nil {
			bucket, ok = sc.trackers[ev.From.String()]
			if ok && bucket != nil {
				bucket.VisitTransaction(tid, func(t *Transaction) {
					if t == nil {
						log.Warnf("no transaction %d", tid)
					} else {
						if t.GotData(ev.Data) {
							err := t.Sync()
							if err != nil {
								log.Warnf("failed to sync swarm: %s", err)
							}
							t.Done()
							log.Debugf("transaction %d done", tid)
						} else {
							sc.sendQueue <- t.SendEvent(ev.From)
						}
					}
				})
			} else {
				log.Warnf("bucket not found for %s", ev.From)
			}
		}

	}
	return
}

func (sc *Scraper) Run() {
	for {
		<-sc.ticker.C
		sc.Scrape()
	}
}

func (sc *Scraper) Scrape() {
	now := time.Now().Add(0 - sc.interval)

	rows, err := db.ORM.Raw("SELECT torrent_id, torrent_hash FROM torrents WHERE last_scrape IS NULL OR last_scrape < ? ORDER BY torrent_id DESC LIMIT 700", now).Rows()
	if err == nil {
		counter := 0
		var scrape [70]model.Torrent
		for rows.Next() {
			idx := counter % 70
			rows.Scan(&scrape[idx].ID, &scrape[idx].Hash)
			counter++
			if idx == 0 {
				for _, b := range sc.trackers {
					t := b.NewTransaction(scrape[:])
					sc.sendQueue <- t.SendEvent(b.Addr)
				}
			}
		}
		rows.Close()

	} else {
		log.Warnf("failed to select torrents for scrape: %s", err)
	}
}

func (sc *Scraper) Wait() {
	<-sc.done
}
