// The boys command runs my boys' room's LED strip.
//
// This is a fork of my bar lights which is a fork of my roof
// lights.
//
// Raspberry Pi pins:
//
//	red: 5V, pin2
//	black: ground, pin6 etc
//	green: SPIO_MOSI, pin19
//	yellow: SPIO_SLCK, pin23
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"periph.io/x/periph/conn/physic"
	"periph.io/x/periph/conn/spi"
	"periph.io/x/periph/conn/spi/spireg"
	"periph.io/x/periph/host"
	"periph.io/x/periph/host/rpi"
)

var theEnv *PaintEnv

// Line is a subset of the lights
type Line struct {
	Low, High int
}

var All Line

func (ln *Line) Foreach(e *PaintEnv, fn func(p Pixel)) {
	for i := ln.Low; i <= ln.High; i++ {
		fn(Pixel{e, i})
	}
}

const (
	startBytes = 4
	stopBytes  = 4 * 4 // good enough
)

// Mem is the memory we send to the SPI device.
// It includes the 4 zero bytes and the variable number of
// 0xff stop bytes.
type Mem []byte

func NewMem(numLights int) Mem {
	return make(Mem, startBytes+numLights*4+stopBytes)
}

// maxBright is the maximum brightness level (31).
// Each pixel can have brightness in range [0,31].
const maxBright = 0xff - 0xe0

// init populates the stop bytes with 0xff and sets
// everything else to black.
func (m Mem) init() {
	s := m[startBytes+*numLights*4:]
	for i := range s {
		s[i] = 0xff
	}
	m.zero()
}

func (m Mem) maybeSetLight(n int, r, g, b, a uint8) {
	if n < 0 || n >= *numLights {
		return
	}
	m.setLight(n, r, g, b, a)
}

func (m Mem) setLight(n int, r, g, b, a uint8) {
	if a > maxBright {
		panic("a too high; max is 31")
	}
	if n >= *numLights {
		panic("n too big")
	}
	if n < 0 {
		panic("negative n")
	}
	s := m[4+n*4:]
	s[0] = 0xe0 + a
	s[1] = b
	s[2] = g
	s[3] = r
}

func (m Mem) zero() {
	for i := 0; i < *numLights; i++ {
		m.setLight(i, 0, 0, 0, 0)
	}
}

func (m Mem) send(c spi.Conn) error {
	err := c.Tx(m[:], nil)
	return err
}

// dieWhenBinaryChanges exits the program when it detects the program
// changed. The Raspberry Pi is running:
//
//	$ while true; do ./xmas ; done
//
// ... in a screen session. And my Makefile on my Mac has:
//
// .PHONY:
//
// xmas: .PHONY
//
//	GOARM=6 GOOS=linux GOARCH=arm go install .
//	scp -C /Users/bradfitz/bin/linux_arm/xmas pi@10.0.0.30:xmas.tmp
//	ssh pi@10.0.0.30 'install xmas.tmp xmas'
//
// So I can just run "make" to updated the house lights within ~2 seconds.
func dieWhenBinaryChanges() {
	fi, err := os.Stat(os.Args[0])
	if err != nil {
		log.Fatal(err)
	}
	mod0 := fi.ModTime()
	for {
		time.Sleep(500 * time.Millisecond)
		if fi, err := os.Stat(os.Args[0]); err != nil || !fi.ModTime().Equal(mod0) {
			log.Printf("modtime changed; dying")
			os.Exit(1)
		}
	}
}

var (
	numLights = flag.Int("n", 450, "number of lights in the strip")
	listen    = flag.String("listen", ":8080", "listen address for HTTP server, empty to not run one")
)

const header = `<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Lights</title>
</head><body>
`

func main() {
	flag.Parse()
	All = Line{Low: 0, High: *numLights - 1}
	if !rpi.Present() {
		log.Fatalf("expected to be running on a Raspberry Pi.")
	}

	st, err := host.Init()
	if err != nil {
		log.Fatalf("host.Init: %v", err)
	}
	log.Printf("running on a Raspberry Pi; state = %#v", st)

	go dieWhenBinaryChanges()

	s, err := spireg.Open("0")
	if err != nil {
		log.Fatalf("spireg.Open: %v", err)
	}
	defer s.Close()

	const hz = 2000000 // 1000000
	spic, err := s.Connect(physic.Frequency(hz)*physic.Hertz, spi.Mode(3), 8)
	if err != nil {
		log.Fatalf("spi Connect: %v", err)
	}

	if p, ok := spic.(spi.Pins); ok {
		log.Printf("Using pins CLK: %s  MOSI: %s  MISO:  %s", p.CLK(), p.MOSI(), p.MISO())
	}

	if flag.NArg() == 1 {
		log.Printf("Debugging light %q", flag.Arg(0))
		targ, _ := strconv.Atoi(flag.Arg(0))
		m := NewMem(*numLights)
		m.init()
		m.setLight(targ, 255, 255, 255, maxBright)
		if err := m.send(spic); err != nil {
			log.Fatalf("send: %v", err)
		}
		return
	}

	theEnv = &PaintEnv{
		dev:  spic,
		Anim: pride{},
		Mem:  NewMem(*numLights),
	}

	go theEnv.readMouseLoop()

	log.Printf("Running...")
	go theEnv.run(nil)

	printMenu := func(w http.ResponseWriter) {
		io.WriteString(w, header+`
<h1><a href="/on">on</a></h1>
<h1><a href="/off">off</a></h1>
<h1><a href="/toggle">toggle</a></h1>
<h1><a href="/ireland">ireland</a></h1>
<h1><a href="/candycane">candycane</a></h1>
</body>
`)
	}

	http.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		printMenu(w)
	}))

	add := func(path string, a Animation) {
		http.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			theEnv.mu.Lock()
			defer theEnv.mu.Unlock()
			theEnv.Cycle = 0
			theEnv.Anim = a
			printMenu(w)
		})
	}

	add("/ireland", ireland{})
	add("/candycane", candyCane{})
	add("/off", off{})
	add("/on", pride{})

	http.Handle("/toggle", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		theEnv.toggleOnOff()
		printMenu(w)
	}))

	if *listen != "" {
		log.Fatal(http.ListenAndServe(*listen, nil))
	}
	select {}
}

func (e *PaintEnv) Sleep(d time.Duration) { e.nextSleep = d }

func (e *PaintEnv) run(cancel <-chan struct{}) {
	e.Mem.init()
	var lastAnim Animation
	for {
		select {
		case <-cancel:
			return
		default:
			e.mu.Lock()
			e.nextSleep = 0
			anim := e.Anim
			if e.Off {
				anim = off{}
			}
			anim.Paint(e)
			if anim != lastAnim {
				lastAnim = anim
				log.Printf("paintEnv running %T", anim)
			}
			e.Cycle++
			d := e.nextSleep
			if err := e.send(e.dev); err != nil {
				log.Fatalf("send: %v", err)
			}
			e.mu.Unlock()
			if d != 0 {
				time.Sleep(d)
			}
		}
	}
}

type Pixel struct {
	e *PaintEnv
	N int
}

func (x Pixel) set(r, g, b, a uint8) {
	x.e.setLight(x.N, r, g, b, a)
}

type PaintEnv struct {
	dev spi.Conn

	mu sync.Mutex // guards rest
	Mem
	Off       bool
	AnimIdx   int
	Cycle     int
	Anim      Animation
	nextSleep time.Duration

	Percent float64 // [0,1]
}

type unit struct{}

type Animation interface {
	Paint(*PaintEnv)
}

type snakeRedGreen struct{}

func (snakeRedGreen) Paint(e *PaintEnv) {
	All.Foreach(e, func(p Pixel) {
		pos := ((p.N - e.Cycle) / 25)
		orig := pos
		pos %= 2
		if pos < 0 {
			pos = -pos
		}

		var r, g, b uint8
		switch pos {
		case 0:
			r = 0xff
		case 1:
			g = 0xff
		default:
			panic("not 0 or 1: " + fmt.Sprintf("%d %% 2 = %d", orig, pos))
		}
		a := (p.N - e.Cycle) % 25
		if a < 0 {
			a = -a
		}
		p.set(r, g, b, 6+byte(a))
	})
}

// randomWhite is a shitty first cut at snow falling on a deep blue
// sky. It needs work.
type randomWhite struct{}

func (randomWhite) Paint(e *PaintEnv) {
	if e.Cycle%2048 == 0 {
		All.Foreach(e, func(p Pixel) {
			p.set(0, 0, 128, maxBright)
		})
	}
	for i := 0; i < 10; i++ {
		n := rand.Intn(*numLights)
		e.setLight(n, 254, 254, 254, maxBright/2)
	}
	for i := 0; i < 20; i++ {
		n := rand.Intn(*numLights)
		e.setLight(n, 254, 254, 254, maxBright)
	}
	for i := 0; i < 10; i++ {
		n := rand.Intn(*numLights)
		e.setLight(n, 0, 0, 128, maxBright)
	}
}

// wreath is a dark green xmas wreath with lights alternating the
// traditional colors.
type wreath struct{}

func (wreath) Paint(e *PaintEnv) {
	if e.Cycle == 0 {
		All.Foreach(e, func(p Pixel) {
			p.set(0, 90, 0, maxBright/7)
		})
	}
	All.Foreach(e, func(p Pixel) {
		if p.N%8 != 0 {
			return
		}
		e := ((e.Cycle + p.N) / 10) % 4
		if e < 0 {
			e = -e
		}
		switch e {
		case 0:
			p.set(255, 0, 0, maxBright)
		case 1:
			p.set(255, 255, 0, maxBright)
		case 2:
			p.set(0, 0, 255, maxBright)
		case 3:
			p.set(255, 0, 255, maxBright)
		}
	})
}

// traditional tries to match the light pattern of the neighbors
// across the street.
type traditional struct{}

func (traditional) Paint(e *PaintEnv) {
	const onWid = 2
	const offWid = 5
	const cols = 4
	var col int
	var bright uint8
	All.Foreach(e, func(p Pixel) {
		what := p.N % (onWid + offWid)
		on := what < onWid
		if on {
			if what == 0 {
				col++
				col %= cols
				bright = 10 + byte(rand.Intn(20))
			}
			switch col {
			case 0:
				p.set(255, 0, 0, bright)
			case 1:
				p.set(255, 200, 0, bright)
			case 2:
				p.set(0, 255, 0, bright)
			case 3:
				p.set(0, 0, 255, bright)
			}
		} else {
			p.set(0, 0, 0, 0)
		}
	})
}

// HSLToRGB convert a Hue-Saturation-Lightness (HSL) color to sRGB.
//
//	0 <= H < 360,
//	0 <= S <= 1,
//	0 <= L <= 1.
//
// The output sRGB values are scaled between 0 and 1.
//
// This is a copy of https://code.google.com/p/chroma/source/browse/f64/colorspace/hsl.go#53
func HSLToRGB(h, s, l float64) (r, g, b float64) {
	var c float64
	if l <= 0.5 {
		c = 2 * l * s
	} else {
		c = (2 - 2*l) * s
	}
	min := l - 0.5*c
	h -= 360 * math.Floor(h/360)
	h /= 60
	x := c * (1 - math.Abs(h-2*math.Floor(h/2)-1))

	switch int(math.Floor(h)) {
	case 0:
		r = min + c
		g = min + x
		b = min
		break
	case 1:
		r = min + x
		g = min + c
		b = min
		break
	case 2:
		r = min
		g = min + c
		b = min + x
		break
	case 3:
		r = min
		g = min + x
		b = min + c
		break
	case 4:
		r = min + x
		g = min
		b = min + c
		break
	case 5:
		r = min + c
		g = min
		b = min + x
		break
	default:
		r = 0
		g = 0
		b = 0
	}
	return
}

// kippes tries to match my eastward neighbor's light pattern.
type kippes struct{}

func (kippes) Paint(e *PaintEnv) {
	All.Foreach(e, func(p Pixel) {
		if p.N%2 == 1 {
			p.set(0, 0, 0, 0)
			return
		}
		l := 0.4
		if rand.Intn(10) == 0 {
			l = 0.6
		}
		s := 1.0
		rf, gf, bf := HSLToRGB(25, s, l)
		r, g, b := byte(rf*255), byte(gf*255), byte(bf*255)
		var a byte = 10
		p.set(r, g, b, a)
	})
}

type candyCane struct{}

func (candyCane) Paint(e *PaintEnv) {
	All.Foreach(e, func(p Pixel) {
		pos := (p.N - e.Cycle) / 16
		if pos < 0 {
			pos = -pos
		}
		orig := pos
		pos %= 2

		var r, g, b uint8
		switch pos {
		case 0:
			r = 0x70
		case 1:
			r, g, b = 0xff, 0xff, 0xff
		default:
			panic("not 0, 1: " + fmt.Sprintf("%d %% 2 = %d", orig, pos))
		}
		p.set(r, g, b, maxBright)
	})
	time.Sleep(32 * time.Millisecond)
}

type particle struct {
	amp       float64
	speedBump float64
}

// fireworks is exploding fireworks.
type fireworks struct {
	origin    int
	lightInc  float64
	hue       int     // 0 to 360
	lnx       float64 // from 1.0 to 20.0 by 0.1
	particles []*particle
	light     []float64
}

func (a *fireworks) Paint(e *PaintEnv) {
	if e.Cycle == 0 {
		*a = fireworks{} // reset
	}
	const maxLn = 8.0
	const lnInc = 0.3

	if a.lnx < 1 || a.lnx > maxLn {
		a.origin = All.Low + rand.Intn(All.High-All.Low)
		a.hue = rand.Intn(360)
		a.lnx = 1.0
		a.particles = a.particles[:0]
		a.lightInc = 0.05
		for i := 0; i < 1000; i++ {
			amp := (rand.Float64() - 0.5) * 2
			amp *= 70
			amp += math.Copysign(1, amp)
			a.particles = append(a.particles, &particle{
				amp:       amp,
				speedBump: 1.0 + rand.Float64()/6,
			})
		}
	}

	a.lnx += lnInc
	a.lightInc -= 0.0015
	if a.lightInc < 0 {
		a.lightInc = 0
	}

	a.light = a.light[:0]
	for i := 0; i < *numLights; i++ {
		a.light = append(a.light, 0.0)
	}

	for _, p := range a.particles {
		x := float64(a.origin) + math.Log(a.lnx)*p.amp*p.speedBump
		xi := int(x)
		if xi < 0 || xi >= *numLights {
			continue
		}
		a.light[xi] += a.lightInc
	}
	All.Foreach(e, func(p Pixel) {
		l := a.light[p.N]
		if l > 1 {
			l = 1
		}
		s := 1.0
		rf, gf, bf := HSLToRGB(float64(a.hue), s, l)
		r, g, b := byte(rf*255), byte(gf*255), byte(bf*255)
		p.set(r, g, b, byte(l*30))
	})
}

type seahawks struct{}

func (seahawks) Paint(e *PaintEnv) {
	//const r1, g1, b1 = 15, 31, 255 // blue
	const r1, g1, b1 = 0, 0, 30  // blue
	const r2, g2, b2 = 0, 167, 1 // green

	for i, seg := range []Line{All} {
		which := i % 2
		var r, g, b, a uint8
		switch which {
		case 0:
			r, g, b, a = r1, g1, b1, maxBright/3*2
		case 1:
			r, g, b, a = r2, g2, b2, maxBright/3
		}
		seg.Foreach(e, func(p Pixel) {
			p.set(r, g, b, a)
		})
	}
	e.Sleep(300 * time.Millisecond) // well, doesn't do anything anyway
}

type pride struct{}

func (pride) Paint(e *PaintEnv) {
	const speed = 1 // lower = slower
	var last Pixel
	All.Foreach(e, func(p Pixel) {
		h := float64((p.N + (e.Cycle * speed)) % 360)
		s := 1.0
		l := 0.5
		rf, gf, bf := HSLToRGB(h, s, l)
		r, g, b := byte(rf*255), byte(gf*255), byte(bf*255)
		p.set(r, g, b, byte(l*30))
		last = p
	})
	last.set(255, 255, 255, maxBright)
	time.Sleep(16 * time.Millisecond)
}

type ireland struct{}

func (ireland) Paint(e *PaintEnv) {
	All.Foreach(e, func(p Pixel) {
		pos := (p.N - e.Cycle) / 16
		if pos < 0 {
			pos = -pos
		}
		orig := pos
		pos %= 4

		var r, g, b uint8
		switch pos {
		case 0:
			g = 0x70
		case 1:
			r, g, b = 0xff, 0xff, 0xff
		case 2:
			r, g = 0x80, 0x40
		case 3:
			r, g, b = 0xff, 0xff, 0xff
		default:
			panic("not 0, 1, 2: " + fmt.Sprintf("%d %% 2 = %d", orig, pos))
		}
		p.set(r, g, b, maxBright)
	})
	time.Sleep(32 * time.Millisecond)
}

// party random!
type random struct{}

func (random) Paint(e *PaintEnv) {
	i := 0
	var h float64
	var r, g, b, a byte
	All.Foreach(e, func(p Pixel) {
		if i%10 == 0 {
			const minHueDelta = 20.0
			h = h + minHueDelta + (rand.Float64() * (360.0 - minHueDelta*2))
			for h > 360 {
				h -= 360
			}
			s := 1.0
			l := 0.5
			rf, gf, bf := HSLToRGB(h, s, l)
			r, g, b = byte(rf*255), byte(gf*255), byte(bf*255)
			a = maxBright
		}
		i++
		e.setLight((p.N+e.Cycle*3)%*numLights, r, g, b, a)
	})
	e.Sleep(300 * time.Millisecond)
}

type off struct{}

func (off) Paint(e *PaintEnv) {
	All.Foreach(e, func(p Pixel) {
		e.setLight(p.N, 0, 0, 0, 0)
	})
	e.Sleep(200 * time.Millisecond)
}

type grower struct {
	Origin int
	Size   int
	Hue    float64 // [0, 360)
}

type colorPlosion struct {
	m       map[*grower]bool
	growers []*grower // newest ones at the end
}

const maxGrowers = 10

func (a *colorPlosion) Paint(e *PaintEnv) {
	if e.Cycle == 0 {
		All.Foreach(e, func(p Pixel) {
			p.set(0, 0, 0, 0)
		})
		a.m = make(map[*grower]bool)
	}
	if len(a.m) < maxGrowers {
		gr := &grower{
			Origin: rand.Intn(*numLights),
			Hue:    rand.Float64() * 360,
		}
		a.m[gr] = true
		a.growers = append(a.growers, gr)

		// Trim a.growers to only be the active ones.
		newg := a.growers[:0]
		for _, gr := range a.growers {
			if _, ok := a.m[gr]; ok {
				newg = append(newg, gr)
			}
		}
		a.growers = newg
	}
	for _, gr := range a.growers {
		gr.Size += 2
		const maxSize = 80
		if gr.Size > maxSize {
			delete(a.m, gr)
			continue
		}
		rf, gf, bf := HSLToRGB(gr.Hue, 1.0, 0.5)
		r, g, b := byte(rf*255), byte(gf*255), byte(bf*255)
		for s := 0; s <= gr.Size; s++ {
			e.maybeSetLight(gr.Origin+s, r, g, b, maxBright)
			e.maybeSetLight(gr.Origin-s, r, g, b, maxBright)
		}
	}
}

type fader struct {
	final Mem
}

func (e *PaintEnv) readMouseLoop() {
	for {
		f, err := os.Open("/dev/input/event0")
		if err != nil {
			log.Printf("open mouse: %v", err)
		} else {
			err = e.processMouseEvents(f)
			log.Printf("processMouseEvents done: %v", err)
		}
		time.Sleep(5 * time.Second)
	}
}

func (e *PaintEnv) toggleOnOff() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Off = !e.Off
}

func (e *PaintEnv) nextAnim() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.AnimIdx++
	animations := []Animation{
		pride{},
		ireland{},
		candyCane{},
	}
	e.Anim = animations[e.AnimIdx%len(animations)]
	e.Cycle = 0
}

func (e *PaintEnv) processMouseEvents(f *os.File) error {
	defer f.Close()
	for {
		ev, err := readInputEvent(f)
		if err != nil {
			return fmt.Errorf("read input event: %v", err)
		}
		if ev.Type == 1 { // EV_KEY button
			if ev.Code == 272 && ev.Value == 1 { // left button press
				e.nextAnim()
			}
			if ev.Code == 274 && ev.Value == 1 { // right button press
				e.toggleOnOff()
			}
		}
	}
}

func readInputEvent(r io.Reader) (inputEvent, error) {
	var e inputEvent
	if unsafe.Sizeof(int(0)) == 4 {
		return inputEvent{}, fmt.Errorf("32-bit architectures not tested/supported")
	}
	if err := binary.Read(r, binary.NativeEndian, &e); err != nil {
		return inputEvent{}, err
	}
	return e, nil
}

type inputEvent struct {
	Sec   int64  // timeval.tv_sec
	Usec  int64  // timeval.tv_usec
	Type  uint16 // ev.type
	Code  uint16 // ev.code
	Value int32  // ev.value
}

func (e inputEvent) Timestamp() time.Time {
	return time.Unix(e.Sec, e.Usec*1_000)
}
