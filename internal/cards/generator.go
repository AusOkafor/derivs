package cards

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// Alert card dimensions
const (
	cardWidth  = 800
	cardHeight = 420
)

// Colors
var (
	bgColor      = color.RGBA{8, 13, 26, 255}   // #080d1a
	accentGreen  = color.RGBA{0, 220, 130, 255}  // #00DC82
	accentRed    = color.RGBA{239, 68, 68, 255} // #EF4444
	accentOrange = color.RGBA{249, 115, 22, 255} // #F97316
	accentYellow = color.RGBA{234, 179, 8, 255}  // #EAB308
	textPrimary  = color.RGBA{255, 255, 255, 255} // white
	textMuted    = color.RGBA{100, 116, 139, 255} // slate-500
	borderColor  = color.RGBA{30, 41, 59, 255}   // slate-800
)

type AlertCardData struct {
	Symbol       string
	Severity     string  // "HIGH", "MEDIUM"
	AlertType    string  // "Liquidity Sweep", "Mixed Zone", "Funding Extreme"
	Price        float64
	ClusterPrice float64
	ClusterSize  float64
	Distance     float64 // in decimal form (0.015 = 1.5%)
	SweepProb    int
	CascadeLevel string
	CascadeScore int
	GravityDir   string
	GravityPct   float64
	Funding      float64
	OIChange     float64
}

func GenerateAlertCard(data AlertCardData) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, cardWidth, cardHeight))

	// Fill background
	fillRect(img, 0, 0, cardWidth, cardHeight, bgColor)

	// Border
	drawBorder(img, borderColor)

	// Top accent line
	severityColor := getSeverityColor(data.Severity)
	fillRect(img, 0, 0, cardWidth, 4, severityColor)

	// Severity badge
	badgeText := fmt.Sprintf("● %s ALERT", data.Severity)
	drawText(img, 40, 35, badgeText, severityColor, false)

	// Symbol + Alert type
	symbolText := fmt.Sprintf("%s — %s", data.Symbol, data.AlertType)
	drawText(img, 40, 70, symbolText, textPrimary, true)

	// Divider line
	fillRect(img, 40, 90, cardWidth-40, 91, borderColor)

	// Left column — price data
	drawText(img, 40, 125, "Current Price", textMuted, false)
	drawText(img, 40, 148, formatPrice(data.Price), textPrimary, true)

	drawText(img, 40, 190, "Cluster Level", textMuted, false)
	drawText(img, 40, 213, formatPrice(data.ClusterPrice), severityColor, true)

	drawText(img, 40, 255, "Distance", textMuted, false)
	drawText(img, 40, 278, fmt.Sprintf("%.2f%%", data.Distance*100), textPrimary, false)

	drawText(img, 40, 320, "Cluster Size", textMuted, false)
	drawText(img, 40, 343, formatUSD(data.ClusterSize), textPrimary, false)

	// Center divider
	fillRect(img, cardWidth/2-1, 100, cardWidth/2, cardHeight-40, borderColor)

	// Right column — signal data
	cx := cardWidth/2 + 40

	drawText(img, cx, 125, "Sweep Probability", textMuted, false)
	probColor := getProbColor(data.SweepProb)
	drawText(img, cx, 148, fmt.Sprintf("%d%%", data.SweepProb), probColor, true)

	// Probability bar
	drawProgressBar(img, cx, 158, 300, 8, data.SweepProb, 100, probColor)

	drawText(img, cx, 200, "Cascade Risk", textMuted, false)
	cascadeColor := getCascadeColor(data.CascadeLevel)
	drawText(img, cx, 223, fmt.Sprintf("%s  %d/100", data.CascadeLevel, data.CascadeScore), cascadeColor, false)

	drawText(img, cx, 265, "Liquidity Gravity", textMuted, false)
	gravityText := fmt.Sprintf("%.1f%% %s", data.GravityPct, data.GravityDir)
	drawText(img, cx, 288, gravityText, accentGreen, false)

	drawText(img, cx, 330, "Funding", textMuted, false)
	fundingText := fmt.Sprintf("%.4f%%", data.Funding*100)
	fundingColor := textPrimary
	if data.Funding > 0.0003 {
		fundingColor = accentRed
	} else if data.Funding < -0.0003 {
		fundingColor = accentGreen
	}
	drawText(img, cx, 353, fundingText, fundingColor, false)

	// Bottom bar
	fillRect(img, 0, cardHeight-50, cardWidth, cardHeight-49, borderColor)

	// Bottom left — derivlens.io branding
	drawText(img, 40, cardHeight-20, "DerivLens", accentGreen, false)
	drawText(img, 140, cardHeight-20, "— Crypto Derivatives Intelligence", textMuted, false)

	// Bottom right — derivlens.io
	drawText(img, cardWidth-160, cardHeight-20, "derivlens.io", textMuted, false)

	// Encode to PNG
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func fillRect(img *image.RGBA, x1, y1, x2, y2 int, c color.RGBA) {
	for x := x1; x < x2; x++ {
		for y := y1; y < y2; y++ {
			img.Set(x, y, c)
		}
	}
}

func drawBorder(img *image.RGBA, c color.RGBA) {
	// 1px border
	for x := 0; x < cardWidth; x++ {
		img.Set(x, 0, c)
		img.Set(x, cardHeight-1, c)
	}
	for y := 0; y < cardHeight; y++ {
		img.Set(0, y, c)
		img.Set(cardWidth-1, y, c)
	}
}

func drawText(img *image.RGBA, x, y int, text string, col color.RGBA, large bool) {
	face := basicfont.Face7x13
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(col),
		Face: face,
		Dot:  fixed.Point26_6{X: fixed.I(x), Y: fixed.I(y)},
	}
	if large {
		// Draw twice offset by 1px for bold effect
		d.DrawString(text)
		d.Dot = fixed.Point26_6{X: fixed.I(x + 1), Y: fixed.I(y)}
		d.DrawString(text)
	} else {
		d.DrawString(text)
	}
}

func drawProgressBar(img *image.RGBA, x, y, width, height, value, max int, col color.RGBA) {
	// Background
	fillRect(img, x, y, x+width, y+height, borderColor)
	// Fill
	filled := int(math.Round(float64(width) * float64(value) / float64(max)))
	if filled > 0 {
		fillRect(img, x, y, x+filled, y+height, col)
	}
}

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
