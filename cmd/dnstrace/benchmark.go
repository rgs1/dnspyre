package dnstrace

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
	"github.com/fatih/color"
	"github.com/miekg/dns"
	"github.com/tantalor93/doh-go/doh"
	"go.uber.org/ratelimit"
	"golang.org/x/net/http2"
)

const dnsTimeout = time.Second * 4

// ResultStats representation of benchmark results of single concurrent thread
type ResultStats struct {
	Codes   map[int]int64
	Qtypes  map[string]int64
	Hist    *hdrhistogram.Histogram
	Timings []Datapoint

	Count     int64
	Cerror    int64
	Ecount    int64
	Success   int64
	Matched   int64
	Mismatch  int64
	Truncated int64
}

func (r *ResultStats) record(time time.Time, timing time.Duration) {
	r.Hist.RecordValue(timing.Nanoseconds())
	r.Timings = append(r.Timings, Datapoint{float64(timing.Milliseconds()), time})
}

// Datapoint one datapoint of benchmark (single DNS request)
type Datapoint struct {
	Duration float64
	Start    time.Time
}

// Benchmark is representation of benchmark scenario
type Benchmark struct {
	Server      string
	Types       []string
	Count       int64
	Concurrency uint32

	Rate     int
	QperConn int64

	ExpectResponseType []string

	Recurse bool

	Probability float64

	UDPSize uint16
	EdnsOpt string

	TCP bool
	DOT bool

	WriteTimeout time.Duration
	ReadTimeout  time.Duration

	Rcodes bool

	HistDisplay bool
	HistMin     time.Duration
	HistMax     time.Duration
	HistPre     int

	Csv string

	Ioerrors bool

	Silent bool
	Color  bool

	PlotDir    string
	PlotFormat string

	DohMethod   string
	DohProtocol string

	Queries []string

	// internal variable so we do not have to parse the address with each request
	useDoH bool
}

func (b *Benchmark) normalize() {
	b.useDoH = strings.HasPrefix(b.Server, "http")

	if !strings.Contains(b.Server, ":") && !b.useDoH {
		b.Server += ":53"
	}
}

// Run executes benchmark
func (b *Benchmark) Run(ctx context.Context) []*ResultStats {
	b.normalize()

	color.NoColor = !b.Color

	questions := make([]string, len(b.Queries))
	for i, q := range b.Queries {
		questions[i] = dns.Fqdn(q)
	}

	if !b.Silent {
		fmt.Printf("Using %d hostnames\n", len(b.Queries))
	}

	var qTypes []uint16
	for _, v := range b.Types {
		qTypes = append(qTypes, dns.StringToType[v])
	}

	network := "udp"
	if b.TCP || b.DOT {
		network = "tcp"
	}

	var dohClient doh.Client
	var dohFunc func(context.Context, string, *dns.Msg) (*dns.Msg, error)
	if b.useDoH {
		network = "https"
		var tr http.RoundTripper
		switch b.DohProtocol {
		case "1.1":
			network = network + "/1.1"
			tr = &http.Transport{}
		case "2":
			network = network + "/2"
			tr = &http2.Transport{}
		default:
			network = network + "/1.1"
			tr = &http.Transport{}
		}
		c := http.Client{Transport: tr, Timeout: b.ReadTimeout}
		dohClient = *doh.NewClient(&c)

		switch b.DohMethod {
		case "post":
			network = network + " (POST)"
			dohFunc = dohClient.SendViaPost
		case "get":
			network = network + " (GET)"
			dohFunc = dohClient.SendViaGet
		default:
			network = network + " (POST)"
			dohFunc = dohClient.SendViaPost
		}
	}

	limits := ""
	var limit ratelimit.Limiter
	if b.Rate > 0 {
		limit = ratelimit.New(b.Rate)
		limits = fmt.Sprintf("(limited to %d QPS)", b.Rate)
	}

	if !b.Silent {
		fmt.Printf("Benchmarking %s via %s with %d concurrent requests %s\n", b.Server, network, b.Concurrency, limits)
	}

	stats := make([]*ResultStats, b.Concurrency)

	var wg sync.WaitGroup
	var w uint32
	for w = 0; w < b.Concurrency; w++ {
		st := &ResultStats{Hist: hdrhistogram.New(b.HistMin.Nanoseconds(), b.HistMax.Nanoseconds(), b.HistPre)}
		stats[w] = st
		if b.Rcodes {
			st.Codes = make(map[int]int64)
		}
		st.Qtypes = make(map[string]int64)

		var co *dns.Conn
		var err error
		wg.Add(1)
		go func(st *ResultStats) {
			defer func() {
				if co != nil {
					co.Close()
				}
				wg.Done()
			}()

			// create a new lock free rand source for this goroutine
			rando := rand.New(rand.NewSource(time.Now().Unix()))

			var i int64
			for i = 0; i < b.Count; i++ {
				for _, qt := range qTypes {
					for _, q := range questions {
						if rando.Float64() > b.Probability {
							continue
						}
						var r *dns.Msg
						m := dns.Msg{}
						m.RecursionDesired = b.Recurse
						m.Question = make([]dns.Question, 1)
						question := dns.Question{Qtype: qt, Qclass: dns.ClassINET}
						if ctx.Err() != nil {
							return
						}
						st.Count++

						// instead of setting the question, do this manually for lower overhead and lock free access to id
						question.Name = q
						m.Id = uint16(rando.Uint32())
						m.Question[0] = question
						if limit != nil {
							limit.Take()
						}

						start := time.Now()
						if b.useDoH {
							r, err = dohFunc(ctx, b.Server, &m)
							if err != nil {
								st.Ecount++
								continue
							}
						} else {
							if co != nil && b.QperConn > 0 && i%b.QperConn == 0 {
								co.Close()
								co = nil
							}

							if co == nil {
								co, err = dialConnection(b, &m, st)
								if err != nil {
									continue
								}
							}

							co.SetWriteDeadline(start.Add(b.WriteTimeout))
							if err = co.WriteMsg(&m); err != nil {
								// error writing
								st.Ecount++
								if b.Ioerrors {
									fmt.Fprintln(os.Stderr, "i/o error dialing: ", err)
								}
								co.Close()
								co = nil
								continue
							}

							co.SetReadDeadline(time.Now().Add(b.ReadTimeout))

							r, err = co.ReadMsg()
							if err != nil {
								// error reading
								st.Ecount++
								if b.Ioerrors {
									fmt.Fprintln(os.Stderr, "i/o error dialing: ", err)
								}
								co.Close()
								co = nil
								continue
							}
						}

						st.record(start, time.Since(start))
						b.evaluateResponse(r, &m, st)
					}
				}
			}
		}(st)
	}

	wg.Wait()

	return stats
}

func (b *Benchmark) evaluateResponse(r *dns.Msg, q *dns.Msg, st *ResultStats) {
	if r.Truncated {
		st.Truncated++
	}

	if r.Rcode == dns.RcodeSuccess {
		if r.Id != q.Id {
			st.Mismatch++
			return
		}
		st.Success++

		if expect := b.ExpectResponseType; len(expect) > 0 {
			for _, s := range r.Answer {
				dnsType := dns.TypeToString[s.Header().Rrtype]
				ok := b.isExpected(dnsType)

				if ok {
					st.Matched++
					break
				}
			}
		}
	}

	if st.Codes != nil {
		var c int64
		if v, ok := st.Codes[r.Rcode]; ok {
			c = v
		}
		c++
		st.Codes[r.Rcode] = c
	}
	if st.Qtypes != nil {
		st.Qtypes[dns.TypeToString[q.Question[0].Qtype]]++
	}
}

func (b *Benchmark) isExpected(dnsType string) bool {
	for _, exp := range b.ExpectResponseType {
		if exp == dnsType {
			return true
		}
	}
	return false
}
