package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"strings"
)

// Invented messages in the style of a school community's groups.

var generalLines = []string{
	"Thank you so much!",
	"Got it, thanks for the reminder 🙏",
	"Does anyone know if there's school on Monday?",
	"Will there be parking on the street side this week?",
	"Running about 10 minutes late, sorry!",
	"Can someone share the sign-up link again?",
	"Thanks everyone for helping out today ❤️",
	"Is the meeting still at 7pm?",
	"Count me in",
	"We found a blue water bottle in the hallway. Whose is it?",
	"Happy to help if you still need someone",
	"See you all on Sunday!",
}

// groupLines are keyed by grouplist.Key of the group name.
var groupLines = map[string][]string{
	// Most announcements are about events, with dates: see events.go.
	"announcements": {
		"Tomorrow please drop off and pick up kids through the backyard gate.",
		"Tuition for next month is due by the 5th. Thank you!",
		"Please label your child's jacket and lunch box. The lost and found is overflowing!",
		"Thank you to all the volunteers who helped this week ❤️",
	},
	"family connection": {
		"Hello families, the Sunday School Director from Resurrection Church extended an invitation to our families.",
		"Welcome to our new families joining this month!",
		"Prayer requests can be shared here or sent to Melissa directly.",
	},
	"preschool": {
		"What a wonderful week learning about Noah and the ark! Our kids enjoyed singing together.",
		"Please pack a change of clothes in your child's backpack.",
		"Snack this week is apples and crackers. Let us know about any allergies.",
		"Show and tell on Friday: something that starts with the letter B!",
	},
	"preschool p.e. teachers": {
		"I can take the Thursday slot next week",
		"Can someone swap with me on 10/23?",
		"We need more cones for the relay game",
	},
	"cleanup duties/campus security": {
		"I'll do the classroom cleanup after pickup today.",
		"Oh ok, thanks. I was going to do it today.",
		"Who has the gate key this week?",
	},
	"yearbook committee": {
		"Please upload your photos from the picnic to the shared folder.",
		"Draft cover options are ready for a vote.",
		"btw I'm getting started on the class pages this weekend",
	},
	"spiritual matters": {
		"Chapel this week will be led by the 3rd graders.",
		"Please read the Code of Conduct before Friday's meeting.",
	},
	"anything to do with high tech": {
		"There's an AI virtual conference for educators this Saturday that may interest some of you.",
		"Has anyone tried the new reading app for the kids?",
	},
	"acorn onsite helper": {
		"Next week's on-site helper schedule is posted. Please check your day.",
		"I can cover Wednesday if anyone needs a swap.",
	},
	"someone opens the door": {
		"Can someone open the door please?",
		"At the front gate now",
		"Coming!",
	},
	"elementary classes": {
		"Anyone want to join a group order for the science kits?",
		"Spelling test moved to Thursday.",
		"Book report due next Monday.",
	},
	"acorn": {
		"I guess David and Melissa volunteered to be the teachers tonight 🥰",
		"Class starts at 6:30 tonight, see you there.",
	},
	"facilities & outdoor": {
		"The basketball court repainting is scheduled for next weekend.",
		"The playground slide has a loose bolt. I've taped it off for now.",
	},
	"seth garden volunteers": {
		"Tomatoes are ready to harvest! Kids can take some home.",
		"Watering schedule for next week is posted.",
	},
	"campus safety": {
		"Fire drill tomorrow at 10am. Please remind the kids it's only practice.",
		"Visitors, please sign in at the front office.",
	},
}

var captions = []string{
	"Field trip fun!", "Our art projects this week", "Picture day", "Science fair setup",
	"Garden harvest", "Chapel this morning", "Snack time", "Look what we built!",
}

var documents = []string{
	"Code of Conduct 1 and 2.pdf", "October newsletter.pdf", "Field trip permission slip.pdf",
	"Volunteer schedule.pdf", "Lunch menu.pdf", "Supply list.pdf",
}

var editNotes = []string{"time changed to 3:30", "corrected the date", "added the room number"}

// jpegImage draws a small simple picture: a sky and grass in a colour chosen
// from the group name, with a few circles, so photos look different.
func jpegImage(rng *rand.Rand, groupName string) []byte {
	const w, h = 480, 360
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	hash := fnv.New32a()
	hash.Write([]byte(groupName))
	hue := hash.Sum32()
	sky := color.RGBA{uint8(150 + hue%80), uint8(190 + hue>>8%50), 240, 255}
	grass := color.RGBA{uint8(80 + hue>>16%60), 160, 70, 255}
	for y := 0; y < h; y++ {
		c := sky
		if y > h*2/3 {
			c = grass
		}
		for x := 0; x < w; x++ {
			im.Set(x, y, c)
		}
	}
	for i := 0; i < 3+rng.Intn(4); i++ {
		cx, cy, r := rng.Intn(w), rng.Intn(h), 15+rng.Intn(40)
		c := color.RGBA{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255}
		for y := cy - r; y <= cy+r; y++ {
			for x := cx - r; x <= cx+r; x++ {
				if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r && x >= 0 && y >= 0 && x < w && y < h {
					im.Set(x, y, c)
				}
			}
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, im, &jpeg.Options{Quality: 80})
	return buf.Bytes()
}

// pdfDocument builds a valid one-page PDF with a title and a line of text.
func pdfDocument(title, group, author string) []byte {
	esc := strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`)
	stream := fmt.Sprintf("BT /F1 20 Tf 72 720 Td (%s) Tj ET\nBT /F1 12 Tf 72 690 Td (Shared in %s by %s. Simulated by seth-sim.) Tj ET",
		esc.Replace(asciiOnly(title)), esc.Replace(asciiOnly(group)), esc.Replace(asciiOnly(author)))
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, o := range objects {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return b.Bytes()
}

func asciiOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r > 126 {
			return -1
		}
		return r
	}, s)
}

// voicePlaceholder stands in for a voice note. It isn't playable audio: it's a
// short text file, so clients can test finding and handling the file, but
// anything that decodes audio should expect it to fail.
func voicePlaceholder(seconds int) []byte {
	return []byte(fmt.Sprintf("seth-sim placeholder for a %d-second voice note. Not real audio.\n", seconds))
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
