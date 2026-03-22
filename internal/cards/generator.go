package cards

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Card dimensions — 1600×840, standard OG-image aspect ratio
const (
	cardWidth  = 1600
	cardHeight = 840
	padX       = 72 // horizontal padding
)

// ── Font faces ────────────────────────────────────────────────────────────────
// All set at 96 DPI (standard screen DPI) to keep rendered sizes predictable.
//
//	faceSmall  = regular 13pt  → ~17px em, ascent ~14px  (labels, meta)
//	faceLarge  = bold    28pt  → ~37px em, ascent ~31px  (values)
//	faceTitle  = bold    34pt  → ~45px em, ascent ~37px  (symbol header)
var (
	faceSmall font.Face
	faceLarge font.Face
	faceTitle font.Face
)

func init() {
	reg, err := opentype.Parse(goregular.TTF)
	if err != nil {
		log.Printf("[cards] failed to parse regular font: %v", err)
	} else {
		faceSmall, err = opentype.NewFace(reg, &opentype.FaceOptions{
			Size: 13, DPI: 96, Hinting: font.HintingFull,
		})
		if err != nil {
			log.Printf("[cards] failed to create faceSmall: %v", err)
		}
	}

	bld, err := opentype.Parse(gobold.TTF)
	if err != nil {
		log.Printf("[cards] failed to parse bold font: %v", err)
		return
	}
	faceLarge, err = opentype.NewFace(bld, &opentype.FaceOptions{
		Size: 28, DPI: 96, Hinting: font.HintingFull,
	})
	if err != nil {
		log.Printf("[cards] failed to create faceLarge: %v", err)
	}
	faceTitle, err = opentype.NewFace(bld, &opentype.FaceOptions{
		Size: 34, DPI: 96, Hinting: font.HintingFull,
	})
	if err != nil {
		log.Printf("[cards] failed to create faceTitle: %v", err)
	}
}

// ── Colors ────────────────────────────────────────────────────────────────────
var (
	bgColor      = color.RGBA{8, 13, 26, 255}    // #080d1a
	accentGreen  = color.RGBA{0, 220, 130, 255}   // #00DC82
	accentRed    = color.RGBA{239, 68, 68, 255}   // #EF4444
	accentOrange = color.RGBA{249, 115, 22, 255}  // #F97316
	accentYellow = color.RGBA{234, 179, 8, 255}   // #EAB308
	textPrimary  = color.RGBA{255, 255, 255, 255} // white
	textMuted    = color.RGBA{100, 116, 139, 255} // slate-500
	borderColor  = color.RGBA{30, 41, 59, 255}    // slate-800
)

// ── Public types ─────────────────────────────────────────────────────────────

type AlertCardData struct {
	Symbol       string
	Severity     string  // "HIGH", "MEDIUM"
	AlertType    string  // e.g. "Liquidity Watch"
	Price        float64
	ClusterPrice float64
	ClusterSize  float64
	Distance     float64 // decimal (0.015 = 1.5%)
	SweepProb    int
	CascadeLevel string
	CascadeScore int
	GravityDir   string
	GravityPct   float64
	Funding      float64
	OIChange     float64
}

// ── Entry point ──────────────────────────────────────────────────────────────

func GenerateAlertCard(data AlertCardData) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, cardWidth, cardHeight))

	fillRect(img, 0, 0, cardWidth, cardHeight, bgColor)
	drawBorder(img, borderColor)

	sevColor := getSeverityColor(data.Severity)

	// ── Top accent bar ───────────────────────────────────────────────────────
	fillRect(img, 0, 0, cardWidth, 6, sevColor)

	// ── Header ───────────────────────────────────────────────────────────────
	// "● HIGH ALERT"  y=75  (faceSmall, ascent≈14, top≈61)
	badge := fmt.Sprintf("● %s ALERT", data.Severity)
	drawWith(img, padX, 75, badge, sevColor, faceSmall)

	// "BTC — Liquidity Watch"  y=140  (faceTitle, ascent≈37, top≈103)
	// gap from badge bottom (≈78) to value top (103) = 25px ✓
	title := fmt.Sprintf("%s — %s", data.Symbol, data.AlertType)
	drawWith(img, padX, 140, title, textPrimary, faceTitle)

	// Divider  y=165  (title bottom≈148, gap=17px ✓)
	fillRect(img, padX, 165, cardWidth-padX, 166, borderColor)

	// ── Column setup ─────────────────────────────────────────────────────────
	cx := cardWidth/2 + padX // right column x origin

	// Vertical center divider
	fillRect(img, cardWidth/2-1, 165, cardWidth/2, cardHeight-70, borderColor)

	// ── Layout rows (verified no-overlap) ────────────────────────────────────
	// Each row: label y, value y, thin-divider y
	// faceSmall ascent≈14, descent≈3  → rendered bottom = baselineY + 3
	// faceLarge ascent≈31, descent≈6  → rendered top    = baselineY - 31

	// Row 1: y=215 label, y=270 value, y=295 divider
	// Row 2: y=325 label, y=380 value, y=405 divider
	// Row 3: y=435 label, y=490 value, y=515 divider
	// Row 4: y=545 label, y=600 value
	//
	// Gaps verified:
	//   label bottom (215+3=218) → value top (270-31=239): 21px ✓
	//   value bottom (270+6=276) → divider (295):           19px ✓
	//   divider (296)            → label top (325-14=311):  15px ✓

	// ── Left column ──────────────────────────────────────────────────────────
	drawWith(img, padX, 215, "CURRENT PRICE", textMuted, faceSmall)
	drawWith(img, padX, 270, formatPrice(data.Price), textPrimary, faceLarge)
	fillRect(img, padX, 295, cardWidth/2-padX, 296, borderColor)

	drawWith(img, padX, 325, "CLUSTER LEVEL", textMuted, faceSmall)
	drawWith(img, padX, 380, formatPrice(data.ClusterPrice), sevColor, faceLarge)
	fillRect(img, padX, 405, cardWidth/2-padX, 406, borderColor)

	drawWith(img, padX, 435, "DISTANCE", textMuted, faceSmall)
	distStr := fmt.Sprintf("%.2f%%", data.Distance*100)
	if data.Distance*100 < 0.01 {
		distStr = "< 0.01%"
	}
	drawWith(img, padX, 490, distStr, textPrimary, faceLarge)
	fillRect(img, padX, 515, cardWidth/2-padX, 516, borderColor)

	drawWith(img, padX, 545, "CLUSTER SIZE", textMuted, faceSmall)
	drawWith(img, padX, 600, formatUSD(data.ClusterSize), textPrimary, faceLarge)

	// ── Right column ─────────────────────────────────────────────────────────
	drawWith(img, cx, 215, "SWEEP PROBABILITY", textMuted, faceSmall)
	probColor := getProbColor(data.SweepProb)
	drawWith(img, cx, 270, fmt.Sprintf("%d%%", data.SweepProb), probColor, faceLarge)
	// Progress bar directly below value (value bottom≈276, bar at 285)
	barW := cardWidth - cx - padX
	drawProgressBar(img, cx, 285, barW, 18, data.SweepProb, 100, probColor)
	// Divider at 315 (bar bottom=303, gap=12px ✓)
	fillRect(img, cx, 315, cardWidth-padX, 316, borderColor)

	drawWith(img, cx, 346, "CASCADE RISK", textMuted, faceSmall)
	cascadeColor := getCascadeColor(data.CascadeLevel)
	drawWith(img, cx, 401, fmt.Sprintf("%s  %d/100", data.CascadeLevel, data.CascadeScore), cascadeColor, faceLarge)
	fillRect(img, cx, 426, cardWidth-padX, 427, borderColor)

	drawWith(img, cx, 456, "LIQUIDITY GRAVITY", textMuted, faceSmall)
	gravStr := fmt.Sprintf("%.1f%% %s", data.GravityPct, data.GravityDir)
	drawWith(img, cx, 511, gravStr, accentGreen, faceLarge)
	fillRect(img, cx, 536, cardWidth-padX, 537, borderColor)

	drawWith(img, cx, 566, "FUNDING RATE", textMuted, faceSmall)
	fundingStr := fmt.Sprintf("%.4f%%", data.Funding*100)
	fundingColor := textPrimary
	if data.Funding > 0.0003 {
		fundingColor = accentRed
	} else if data.Funding < -0.0003 {
		fundingColor = accentGreen
	}
	drawWith(img, cx, 621, fundingStr, fundingColor, faceLarge)

	// ── Bottom bar ───────────────────────────────────────────────────────────
	fillRect(img, 0, cardHeight-70, cardWidth, cardHeight-69, borderColor)
	drawWith(img, padX, cardHeight-30, "DerivLens", accentGreen, faceSmall)
	drawWith(img, cardWidth/2-90, cardHeight-30, "Crypto Derivatives Intelligence", textMuted, faceSmall)
	drawWith(img, cardWidth-180, cardHeight-30, "derivlens.io", textMuted, faceSmall)

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ── Drawing helpers ───────────────────────────────────────────────────────────

func drawWith(img *image.RGBA, x, y int, text string, col color.RGBA, face font.Face) {
	if face == nil {
		// Bitmap fallback — only fires if font loading failed at startup
		d := &font.Drawer{
			Dst:  img,
			Src:  image.NewUniform(col),
			Face: basicfont.Face7x13,
			Dot:  fixed.Point26_6{X: fixed.I(x), Y: fixed.I(y)},
		}
		d.DrawString(text)
		return
	}
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(col),
		Face: face,
		Dot:  fixed.Point26_6{X: fixed.I(x), Y: fixed.I(y)},
	}
	d.DrawString(text)
}

func fillRect(img *image.RGBA, x1, y1, x2, y2 int, c color.RGBA) {
	for x := x1; x < x2; x++ {
		for y := y1; y < y2; y++ {
			img.Set(x, y, c)
		}
	}
}

func drawBorder(img *image.RGBA, c color.RGBA) {
	for x := 0; x < cardWidth; x++ {
		img.Set(x, 0, c)
		img.Set(x, cardHeight-1, c)
	}
	for y := 0; y < cardHeight; y++ {
		img.Set(0, y, c)
		img.Set(cardWidth-1, y, c)
	}
}

func drawProgressBar(img *image.RGBA, x, y, width, height, value, max int, col color.RGBA) {
	fillRect(img, x, y, x+width, y+height, borderColor)
	filled := int(math.Round(float64(width) * float64(value) / float64(max)))
	if filled > 0 {
		fillRect(img, x, y, x+filled, y+height, col)
	}
}

// ── Color helpers ─────────────────────────────────────────────────────────────

func getSeverityColor(severity string) color.RGBA {
	switch severity {
	case "HIGH", "high":
		return accentRed
	case "MEDIUM", "medium":
		return accentOrange
	default:
		return accentYellow
	}
}

func getProbColor(prob int) color.RGBA {
	if prob >= 80 {
		return accentRed
	} else if prob >= 60 {
		return accentOrange
	}
	return accentYellow
}

func getCascadeColor(level string) color.RGBA {
	switch level {
	case "CRITICAL":
		return accentRed
	case "HIGH":
		return accentOrange
	case "MEDIUM":
		return accentYellow
	default:
		return textMuted
	}
}

// ── Format helpers ────────────────────────────────────────────────────────────

func formatPrice(p float64) string {
	if p >= 1000 {
		return fmt.Sprintf("$%.2f", p)
	} else if p >= 1 {
		return fmt.Sprintf("$%.3f", p)
	}
	return fmt.Sprintf("$%.4f", p)
}

func formatUSD(v float64) string {
	if v >= 1_000_000 {
		return fmt.Sprintf("$%.2fM", v/1_000_000)
	} else if v >= 1_000 {
		return fmt.Sprintf("$%.1fK", v/1_000)
	}
	return fmt.Sprintf("$%.0f", v)
}
