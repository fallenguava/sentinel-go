package imaging

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png" // Register PNG decoder
	"log"
	"math"
)

var charPatterns = map[rune][]uint8{
	'0': {0x1E, 0x33, 0x37, 0x3F, 0x3B, 0x33, 0x1E},
	'1': {0x0C, 0x1C, 0x0C, 0x0C, 0x0C, 0x0C, 0x3F},
	'2': {0x1E, 0x33, 0x03, 0x0E, 0x18, 0x30, 0x3F},
	'3': {0x1E, 0x33, 0x03, 0x0E, 0x03, 0x33, 0x1E},
	'4': {0x06, 0x0E, 0x16, 0x26, 0x3F, 0x06, 0x06},
	'5': {0x3F, 0x30, 0x3E, 0x03, 0x03, 0x33, 0x1E},
	'6': {0x0E, 0x18, 0x30, 0x3E, 0x33, 0x33, 0x1E},
	'7': {0x3F, 0x03, 0x06, 0x0C, 0x18, 0x18, 0x18},
	'8': {0x1E, 0x33, 0x33, 0x1E, 0x33, 0x33, 0x1E},
	'9': {0x1E, 0x33, 0x33, 0x1F, 0x03, 0x06, 0x1C},
	'C': {0x1E, 0x33, 0x30, 0x30, 0x30, 0x33, 0x1E},
	'a': {0x00, 0x00, 0x1E, 0x03, 0x1F, 0x33, 0x1F},
	'm': {0x00, 0x00, 0x36, 0x3F, 0x2B, 0x23, 0x23},
	'e': {0x00, 0x00, 0x1E, 0x33, 0x3F, 0x30, 0x1E},
	'r': {0x00, 0x00, 0x2E, 0x33, 0x30, 0x30, 0x30},
	' ': {0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
	'-': {0x00, 0x00, 0x00, 0x3F, 0x00, 0x00, 0x00},
	'.': {0x00, 0x00, 0x00, 0x00, 0x00, 0x18, 0x18},
	':': {0x00, 0x18, 0x18, 0x00, 0x00, 0x18, 0x18},
}

// CapturedImage holds image data with metadata
type CapturedImage struct {
	CamNumber int
	CamName   string
	Data      []byte
	Image     image.Image
}

// CollageConfig holds configuration for collage generation
type CollageConfig struct {
	// Target width for each cell (images will be scaled)
	CellWidth int
	// Target height for each cell
	CellHeight int
	// Padding between images
	Padding int
	// Background color
	BackgroundColor color.Color
	// JPEG quality (1-100)
	Quality int
	// Add labels to images
	ShowLabels bool
}

// DefaultCollageConfig returns sensible defaults for a high-quality collage
func DefaultCollageConfig() *CollageConfig {
	return &CollageConfig{
		CellWidth:       640,                         // Good balance of quality and size
		CellHeight:      480,                         // 4:3 aspect ratio (common for CCTV)
		Padding:         4,                           // Small gap between images
		BackgroundColor: color.RGBA{32, 32, 32, 255}, // Dark gray
		Quality:         90,                          // High quality JPEG
		ShowLabels:      true,
	}
}

// HighQualityCollageConfig returns config for maximum quality
func HighQualityCollageConfig() *CollageConfig {
	return &CollageConfig{
		CellWidth:       800,
		CellHeight:      600,
		Padding:         6,
		BackgroundColor: color.RGBA{24, 24, 24, 255},
		Quality:         95,
		ShowLabels:      true,
	}
}

// CreateCollage combines multiple images into a single collage
// Returns the collage as JPEG bytes
func CreateCollage(images []*CapturedImage, cfg *CollageConfig) ([]byte, error) {
	if len(images) == 0 {
		return nil, fmt.Errorf("no images provided")
	}

	if cfg == nil {
		cfg = DefaultCollageConfig()
	}

	// Decode all images first
	for i, img := range images {
		if img.Image == nil {
			decoded, _, err := image.Decode(bytes.NewReader(img.Data))
			if err != nil {
				log.Printf("[COLLAGE] Failed to decode image for Camera %d: %v", img.CamNumber, err)
				continue
			}
			images[i].Image = decoded
		}
	}

	// Filter out failed decodes
	var validImages []*CapturedImage
	for _, img := range images {
		if img.Image != nil {
			validImages = append(validImages, img)
		}
	}

	if len(validImages) == 0 {
		return nil, fmt.Errorf("no valid images to create collage")
	}

	// Calculate grid dimensions
	cols, rows := calculateGrid(len(validImages))
	log.Printf("[COLLAGE] Creating %dx%d grid for %d images", cols, rows, len(validImages))

	// Calculate total canvas size
	canvasWidth := cols*cfg.CellWidth + (cols+1)*cfg.Padding
	canvasHeight := rows*cfg.CellHeight + (rows+1)*cfg.Padding

	// Create the canvas
	canvas := image.NewRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))

	// Fill with background color
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{cfg.BackgroundColor}, image.Point{}, draw.Src)

	// Place each image in the grid
	for i, img := range validImages {
		col := i % cols
		row := i / cols

		// Calculate position
		x := cfg.Padding + col*(cfg.CellWidth+cfg.Padding)
		y := cfg.Padding + row*(cfg.CellHeight+cfg.Padding)

		// Scale and place the image
		destRect := image.Rect(x, y, x+cfg.CellWidth, y+cfg.CellHeight)
		scaledImg := scaleImage(img.Image, cfg.CellWidth, cfg.CellHeight)
		draw.Draw(canvas, destRect, scaledImg, image.Point{}, draw.Src)

		// Add label overlay if enabled
		if cfg.ShowLabels {
			drawLabel(canvas, destRect, img.CamName)
		}
	}

	// Encode to JPEG
	var buf bytes.Buffer
	opts := &jpeg.Options{Quality: cfg.Quality}
	if err := jpeg.Encode(&buf, canvas, opts); err != nil {
		return nil, fmt.Errorf("failed to encode collage: %w", err)
	}

	log.Printf("[COLLAGE] Created collage: %dx%d, %d bytes", canvasWidth, canvasHeight, buf.Len())
	return buf.Bytes(), nil
}

// calculateGrid determines the optimal grid layout for n images
func calculateGrid(n int) (cols, rows int) {
	switch n {
	case 1:
		return 1, 1
	case 2:
		return 2, 1
	case 3:
		return 3, 1
	case 4:
		return 2, 2
	case 5, 6:
		return 3, 2
	case 7, 8, 9:
		return 3, 3
	case 10, 11, 12:
		return 4, 3
	default:
		// For larger numbers, calculate based on sqrt
		cols = int(math.Ceil(math.Sqrt(float64(n))))
		rows = int(math.Ceil(float64(n) / float64(cols)))
		return cols, rows
	}
}

// scaleImage scales an image to fit within the target dimensions while maintaining aspect ratio
// and centers it on a background
func scaleImage(src image.Image, targetWidth, targetHeight int) image.Image {
	srcBounds := src.Bounds()
	srcWidth := srcBounds.Dx()
	srcHeight := srcBounds.Dy()

	// Calculate scale factor to fit within target while maintaining aspect ratio
	scaleX := float64(targetWidth) / float64(srcWidth)
	scaleY := float64(targetHeight) / float64(srcHeight)
	scale := math.Min(scaleX, scaleY)

	// Calculate new dimensions
	newWidth := int(float64(srcWidth) * scale)
	newHeight := int(float64(srcHeight) * scale)

	// Create destination image
	dst := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))

	// Fill with dark background (in case image doesn't fill the cell)
	draw.Draw(dst, dst.Bounds(), &image.Uniform{color.RGBA{24, 24, 24, 255}}, image.Point{}, draw.Src)

	// Calculate offset to center the image
	offsetX := (targetWidth - newWidth) / 2
	offsetY := (targetHeight - newHeight) / 2

	// Use bilinear scaling for better quality
	bilinearScale(dst, src, offsetX, offsetY, newWidth, newHeight)

	return dst
}

// bilinearScale performs bilinear interpolation scaling
func bilinearScale(dst *image.RGBA, src image.Image, offsetX, offsetY, newWidth, newHeight int) {
	srcBounds := src.Bounds()
	srcWidth := srcBounds.Dx()
	srcHeight := srcBounds.Dy()

	for y := 0; y < newHeight; y++ {
		for x := 0; x < newWidth; x++ {
			// Map destination coordinates to source coordinates
			srcX := float64(x) * float64(srcWidth) / float64(newWidth)
			srcY := float64(y) * float64(srcHeight) / float64(newHeight)

			// Get the four surrounding pixels
			x0 := int(srcX)
			y0 := int(srcY)
			x1 := x0 + 1
			y1 := y0 + 1

			// Clamp to bounds
			if x1 >= srcWidth {
				x1 = srcWidth - 1
			}
			if y1 >= srcHeight {
				y1 = srcHeight - 1
			}

			// Calculate interpolation weights
			xWeight := srcX - float64(x0)
			yWeight := srcY - float64(y0)

			// Get colors of four surrounding pixels
			c00 := src.At(srcBounds.Min.X+x0, srcBounds.Min.Y+y0)
			c10 := src.At(srcBounds.Min.X+x1, srcBounds.Min.Y+y0)
			c01 := src.At(srcBounds.Min.X+x0, srcBounds.Min.Y+y1)
			c11 := src.At(srcBounds.Min.X+x1, srcBounds.Min.Y+y1)

			// Convert to RGBA
			r00, g00, b00, a00 := c00.RGBA()
			r10, g10, b10, a10 := c10.RGBA()
			r01, g01, b01, a01 := c01.RGBA()
			r11, g11, b11, a11 := c11.RGBA()

			// Bilinear interpolation
			r := bilinearInterp(r00, r10, r01, r11, xWeight, yWeight)
			g := bilinearInterp(g00, g10, g01, g11, xWeight, yWeight)
			b := bilinearInterp(b00, b10, b01, b11, xWeight, yWeight)
			a := bilinearInterp(a00, a10, a01, a11, xWeight, yWeight)

			dst.Set(offsetX+x, offsetY+y, color.RGBA{
				R: uint8(r >> 8),
				G: uint8(g >> 8),
				B: uint8(b >> 8),
				A: uint8(a >> 8),
			})
		}
	}
}

// bilinearInterp performs bilinear interpolation on a single channel
func bilinearInterp(c00, c10, c01, c11 uint32, xWeight, yWeight float64) uint32 {
	top := float64(c00)*(1-xWeight) + float64(c10)*xWeight
	bottom := float64(c01)*(1-xWeight) + float64(c11)*xWeight
	return uint32(top*(1-yWeight) + bottom*yWeight)
}

// drawLabel adds a semi-transparent label overlay to an image cell
func drawLabel(canvas *image.RGBA, cellRect image.Rectangle, label string) {
	// Create a semi-transparent overlay at the bottom of the cell
	labelHeight := 28
	labelRect := image.Rect(
		cellRect.Min.X,
		cellRect.Max.Y-labelHeight,
		cellRect.Max.X,
		cellRect.Max.Y,
	)

	// Draw semi-transparent black background
	for y := labelRect.Min.Y; y < labelRect.Max.Y; y++ {
		for x := labelRect.Min.X; x < labelRect.Max.X; x++ {
			existing := canvas.RGBAAt(x, y)
			// Blend with 60% black
			blended := color.RGBA{
				R: uint8(float64(existing.R) * 0.4),
				G: uint8(float64(existing.G) * 0.4),
				B: uint8(float64(existing.B) * 0.4),
				A: 255,
			}
			canvas.Set(x, y, blended)
		}
	}

	// Draw simple text (basic pixel font for camera label)
	// This is a simple approach - for production you might want to use freetype
	drawSimpleText(canvas, labelRect.Min.X+8, labelRect.Min.Y+6, label)
}

// drawSimpleText draws text using a basic bitmap approach
// For a production app, consider using golang.org/x/image/font
func drawSimpleText(canvas *image.RGBA, x, y int, text string) {
	// Simple 5x7 bitmap font for basic characters
	// This is a minimal implementation - just draws white pixels for visibility
	white := color.RGBA{255, 255, 255, 255}

	// For simplicity, we'll just draw a basic indicator
	// In production, use proper font rendering
	charWidth := 8
	charHeight := 14

	for i, ch := range text {
		if i > 20 { // Limit text length
			break
		}
		drawChar(canvas, x+i*charWidth, y, ch, white, charWidth, charHeight)
	}
}

// drawChar draws a single character (simplified bitmap rendering)
func drawChar(canvas *image.RGBA, x, y int, ch rune, col color.RGBA, width, height int) {
	// Simplified character rendering - draws recognizable shapes
	// For production, use freetype or embedded bitmap fonts

	patterns := getCharPattern(ch)
	if patterns == nil {
		return
	}

	for row, pattern := range patterns {
		for col := 0; col < 6; col++ {
			if (pattern>>uint(5-col))&1 == 1 {
				px := x + col
				py := y + row*2
				if px >= 0 && py >= 0 {
					canvas.Set(px, py, color.RGBA{255, 255, 255, 255})
					canvas.Set(px, py+1, color.RGBA{255, 255, 255, 255})
				}
			}
		}
	}
}

// getCharPattern returns a 7-row bitmap pattern for a character
func getCharPattern(ch rune) []uint8 {
	if p, ok := charPatterns[ch]; ok {
		return p
	}

	// Default pattern for unknown chars (small square)
	return []uint8{0x00, 0x1E, 0x12, 0x12, 0x12, 0x1E, 0x00}
}
