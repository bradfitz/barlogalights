// The barlogalights command runs my home bar's LED strips.
//
// This is not code I'm very proud of. It's code I wrote (in
// 2015/2017) while drinking in said bar. It also morphed to and from
// code that I wrote to run the LED strips on my roof at one point.
//
// Buy a Raspberry Pi and 1-5 of these:
//     "Addressable LED Strip APA102-C"
//     (5 meters, 30 LEDs/meter, White PCB, silicone sleeve)
//     http://www.amazon.com/gp/product/B00YVYSOC2
//
// If SPI is disabled on the Raspberry Pi,
// Add dtparam=spi=on to /boot/config.txt and reboot.
//
// Raspberry Pi B+ pins:
//
//    red: 5V, pin2
//    black: ground, pin6 etc
//    yellow: SPIO_MOSI, pin19
//    green: SPIO_SLCK, pin23
package main

import (
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

	"periph.io/x/periph/conn/physic"
	"periph.io/x/periph/conn/spi"
	"periph.io/x/periph/conn/spi/spireg"
	"periph.io/x/periph/host"
	"periph.io/x/periph/host/rpi"
)

// NumLight is the number of lights in the strip.
// Fewer means better refresh rate.
const NumLight = 46 * 4

// CardDir is a cardinal direction.
// It is aspirational only for an unwritten simulator.
type CardDir byte

const (
	_ CardDir = iota
	North
	South
	East
	West
)

var theEnv *PaintEnv

// Line is a subset of the lights on Brad's house.
type Line struct {
	Low, High int
	Dir       CardDir
}

var (
	Short = Line{0, 50, North}
	All   = Line{0, NumLight - 1, North}

	Wall  = Line{22, 81, East}    // back wall on deck
	G1    = Line{82, 126, South}  // east glass panel #1
	G2    = Line{127, 167, South} // ...
	G3    = Line{168, 212, South}
	G4    = Line{213, 255, South}
	G6    = Line{256, 299, South}
	G7    = Line{300, 341, South}
	G8    = Line{342, 385, South}
	G9    = Line{386, 429, South} //east glass panel #9
	G19   = Line{82, 429, South}  // all east glass panels, 1-9
	F5    = Line{430, 462, West}  // rightmost glass panel when facing house (left side is high num)
	F4    = Line{463, 501, West}  // ...
	F3    = Line{502, 538, West}
	F2    = Line{539, 575, West}
	F1    = Line{576, 612, West}  // leftmost glass panel when facing house (left side is high num)
	Front = Line{430, 612, West}  // all front glass panels
	GW    = Line{613, 654, North} // glass part facing west by door
	FW    = Line{655, 705, West}  // front wood (over door)
	WoodW = Line{706, 748, North} // west wall
)

func (ln *Line) Foreach(e *PaintEnv, fn func(p Pixel)) {
	for i := ln.Low; i <= ln.High; i++ {
		fn(Pixel{e, i})
	}
}

const (
	startBytes = 4
	stopBytes  = ((NumLight / 2) / 8) + 1
)

// Mem is the memory we send to the SPI device.
// It includes the 4 zero bytes and the variable number of
// 0xff stop bytes.
type Mem [4 + NumLight*4 + stopBytes]byte

// maxBright is the maximum brightness level (31).
// Each pixel can have brightness in range [0,31].
const maxBright = 0xff - 0xe0

// init populates the stop bytes with 0xff and sets
// everything else to black.
func (m *Mem) init() {
	s := m[4+NumLight*4:]
	for i := range s {
		s[i] = 0xff
	}
	m.zero()
}

func (m *Mem) maybeSetLight(n int, r, g, b, a uint8) {
	if n < 0 || n >= NumLight {
		return
	}
	m.setLight(n, r, g, b, a)
}

func (m *Mem) setLight(n int, r, g, b, a uint8) {
	if a > maxBright {
		panic("a too high; max is 31")
	}
	if n >= NumLight {
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

func (m *Mem) zero() {
	for i := 0; i < NumLight; i++ {
		m.setLight(i, 0, 0, 0, 0)
	}
}

func (m *Mem) send(c spi.Conn) error {
	err := c.Tx(m[:], nil)
	return err
}

// dieWhenBinaryChanges exits the program when it detects the program
// changed. The Raspberry Pi is running:
//
//   $ while true; do ./xmas ; done
//
// ... in a screen session. And my Makefile on my Mac has:
//
// .PHONY:
//
// xmas: .PHONY
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

var listen = flag.String("listen", ":8080", "listen address for HTTP server, empty to not run one")

func main() {
	flag.Parse()
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
		var m Mem
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
	}

	log.Printf("Running...")
	go theEnv.run(nil)

	http.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `<html>
<body>
<h1><a href="/on">on</a></h1>
<h1><a href="/off">off</a></h1>
<h1><a href="/toggle">toggle</a></h1>
</body>
`)
	}))
	http.Handle("/off", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		theEnv.mu.Lock()
		defer theEnv.mu.Unlock()
		log.Printf("switched off")
		theEnv.Anim = off{}
		io.WriteString(w, "<html><body><h1>lights OFF</h1>")
	}))
	http.Handle("/on", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		theEnv.mu.Lock()
		defer theEnv.mu.Unlock()
		theEnv.Anim = pride{}
		io.WriteString(w, "<html><body><h1>lights ON</h1>")
	}))
	http.Handle("/toggle", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		theEnv.mu.Lock()
		defer theEnv.mu.Unlock()
		if _, isOff := theEnv.Anim.(off); isOff {
			theEnv.Anim = pride{}
			log.Printf("switched on")
			io.WriteString(w, "<html><body><h1>lights ON</h1>")
		} else {
			theEnv.Anim = off{}
			log.Printf("switched off")
			io.WriteString(w, "<html><body><h1>lights OFF</h1>")
		}
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
			e.Anim.Paint(e)
			if e.Anim != lastAnim {
				lastAnim = e.Anim
				log.Printf("paintEnv running %T", e.Anim)
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
		n := rand.Intn(NumLight)
		e.setLight(n, 254, 254, 254, maxBright/2)
	}
	for i := 0; i < 20; i++ {
		n := rand.Intn(NumLight)
		e.setLight(n, 254, 254, 254, maxBright)
	}
	for i := 0; i < 10; i++ {
		n := rand.Intn(NumLight)
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

var segs = [...]Line{
	G1, G2, G3, G4, G4, G6, G7, G8, G9,
	F5, F4, F3, F2, F1,
	GW, FW, WoodW,
}

// randomSegs makes each glass segment on the roof alternate between
// red and green at random times.
type randomSegs struct {
	state []bool
	count []int
}

func (a *randomSegs) Paint(e *PaintEnv) {
	if e.Cycle == 0 {
		a.state = make([]bool, len(segs))
		a.count = make([]int, len(segs))
	}
	for i, seg := range segs {
		on := a.state[i]
		if a.count[i] == 0 {
			a.state[i] = !on
			a.count[i] = rand.Intn(20)
		} else {
			a.count[i]--
		}
		seg.Foreach(e, func(p Pixel) {
			if on {
				p.set(255, 0, 0, 30)
			} else {
				p.set(0, 128, 0, 20)
			}
		})
	}
}

// HSLToRGB convert a Hue-Saturation-Lightness (HSL) color to sRGB.
//    0 <= H < 360,
//    0 <= S <= 1,
//    0 <= L <= 1.
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

// candyCane is dark red with oscillating bright white segments around
// each bar on the roof.
type candyCane struct {
	rad   []float64
	amp   []float64
	speed []float64
}

func (a *candyCane) Paint(e *PaintEnv) {
	if e.Cycle == 0 {
		a.rad = make([]float64, len(segs))
		a.amp = make([]float64, len(segs))
		a.speed = make([]float64, len(segs))
		for i := range segs {
			a.rad[i] = rand.Float64() * (math.Pi / 0.5)
			a.amp[i] = float64(10 + rand.Intn(5))
			a.speed[i] = 0.1 + (rand.Float64() / 3)
		}
	}
	All.Foreach(e, func(p Pixel) {
		p.set(90, 0, 0, maxBright/7)
	})
	for i, seg := range segs {
		a.rad[i] += a.speed[i]
		cos := math.Cos(a.rad[i]) * a.amp[i]
		from := seg.Low
		to := seg.Low + int(cos)
		if to < from {
			to, from = from, to
		}
		if from < 0 {
			from = 0
		}
		if to >= NumLight {
			to = NumLight
		}
		for n := from; n <= to; n++ {
			e.setLight(n, 255, 255, 255, maxBright)
		}
	}
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
		a.origin = Front.Low + rand.Intn(Front.High-Front.Low)
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
	for i := 0; i < NumLight; i++ {
		a.light = append(a.light, 0.0)
	}

	for _, p := range a.particles {
		x := float64(a.origin) + math.Log(a.lnx)*p.amp*p.speedBump
		xi := int(x)
		if xi < 0 || xi >= NumLight {
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

	for i, seg := range segs {
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
	All.Foreach(e, func(p Pixel) {
		h := float64((p.N + (e.Cycle * speed)) % 360)
		s := 1.0
		l := 0.5
		rf, gf, bf := HSLToRGB(h, s, l)
		r, g, b := byte(rf*255), byte(gf*255), byte(bf*255)
		p.set(r, g, b, byte(l*30))
	})
	time.Sleep(30 * time.Millisecond)
}

type ireland struct{}

func (ireland) Paint(e *PaintEnv) {
	log.Printf("paint cycle = %d\n", e.Cycle)
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
	time.Sleep(16 * time.Millisecond)
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
		e.setLight((p.N+e.Cycle*3)%NumLight, r, g, b, a)
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
			Origin: rand.Intn(NumLight),
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
