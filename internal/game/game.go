package game

import (
	"fmt"
	"image/color"
	"math"
	"math/rand"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/ebitenutil"
	"github.com/hajimehoshi/ebiten/v2/vector"
)

// borderWidth is the pixel gap between the window edge and the battlefield.
const borderWidth = 24

// hudScale is the integer upscale factor applied to all HUD text (3 = 3× larger).
const hudScale = 3

// overlayColors maps each IntelMapKind to its debug render colour.
var overlayColors = [intelMapCount]color.RGBA{
	IntelContact:          {R: 255, G: 50, B: 50, A: 180},  // bright red
	IntelRecentContact:    {R: 255, G: 140, B: 0, A: 140},  // orange
	IntelThreatDensity:    {R: 200, G: 0, B: 200, A: 120},  // purple
	IntelFriendlyPresence: {R: 30, G: 160, B: 255, A: 130}, // sky blue
	IntelDangerZone:       {R: 255, G: 220, B: 0, A: 140},  // yellow
	IntelUnexplored:       {R: 20, G: 20, B: 20, A: 160},   // dark grey
}

type Game struct {
	width              int
	height             int
	gameWidth          int           // playfield width (log panel takes the rest)
	gameHeight         int           // playfield height (inside border)
	offX               int           // pixel offset from window left to battlefield left
	offY               int           // pixel offset from window top to battlefield top
	buildings          []rect        // individual wall segments (1-cell wide), used for LOS/nav
	windows            []rect        // window segments: block movement, transparent to LOS
	buildingFootprints []rect        // overall floor area of each structure, used for rendering
	roads              []roadSegment // road layout (generated before buildings)
	covers             []*CoverObject
	navGrid            *NavGrid
	soldiers           []*Soldier // red friendlies
	opfor              []*Soldier // blue OpFor
	squads             []*Squad
	thoughtLog         *ThoughtLog
	combat             *CombatManager
	intel              *IntelStore
	tacticalMap        *TacticalMap
	tick               int
	nextID             int

	// Overlay toggle state.
	// showOverlay[team][layer] = visible?
	showOverlay [2][intelMapCount]bool
	overlayTeam int  // 0 = red, 1 = blue (which team's maps are shown)
	showHUD     bool // toggle HUD key labels
	prevKeys    map[ebiten.Key]bool

	// Offscreen buffer for vision cone rendering (avoids additive blowout).
	visionBuf *ebiten.Image
	// Offscreen buffer for the full battlefield — camera transform applied on blit.
	worldBuf *ebiten.Image
	// Offscreen buffer for HUD text — rendered at 1x then blitted at hudScale.
	hudBuf *ebiten.Image

	// Deterministic terrain noise patches, generated once.
	terrainPatches []terrainPatch

	// Camera pan + zoom.
	camX    float64 // world-space X of the camera centre
	camY    float64 // world-space Y of the camera centre
	camZoom float64 // zoom factor (1.0 = native, >1 = zoomed in)

	// Soldier speech bubbles.
	speechBubbles []*SpeechBubble
	speechRng     *rand.Rand

	// Soldier inspector (click-to-select panel).
	inspector     Inspector
	prevMouseLeft bool // for edge-triggered click detection

	// Simulation speed control.
	simSpeed  float64 // multiplier: 0=paused, 0.5, 1, 2, 4
	tickAccum float64 // fractional tick accumulator for sub-1x speeds

	// Analytics reporter — collects behaviour stats periodically.
	reporter *SimReporter
}

type rect struct {
	x int
	y int
	w int
	h int
}

// terrainPatch is a subtle ground colour variation tile.
type terrainPatch struct {
	x, y  float32
	w, h  float32
	shade uint8 // offset from base green
}

func New() *Game {
	// Battlefield is 3072x1728 — double the original size.
	battleW := 3072
	battleH := 1728
	g := &Game{
		width:      borderWidth + battleW + borderWidth + logPanelWidth,
		height:     borderWidth + battleH + borderWidth,
		gameWidth:  battleW,
		gameHeight: battleH,
		offX:       borderWidth,
		offY:       borderWidth,
		thoughtLog: NewThoughtLog(),
		showHUD:    true,
		prevKeys:   make(map[ebiten.Key]bool),
	}
	g.initRoads()
	g.initBuildings()
	g.initCover()
	g.navGrid = NewNavGrid(g.gameWidth, g.gameHeight, g.buildings, soldierRadius, g.covers, g.windows)
	g.tacticalMap = NewTacticalMap(g.gameWidth, g.gameHeight, g.buildings, g.windows, g.buildingFootprints)
	g.initSoldiers()
	g.initOpFor()
	g.initSquads()
	g.randomiseProfiles()
	g.combat = NewCombatManager(time.Now().UnixNano() + 7777)
	g.intel = NewIntelStore(g.gameWidth, g.gameHeight)
	g.visionBuf = ebiten.NewImage(battleW, battleH)
	g.worldBuf = ebiten.NewImage(battleW, battleH)
	// HUD buffer: 1/hudScale of screen so it renders crisply when scaled up.
	g.hudBuf = ebiten.NewImage(g.width/hudScale, g.height/hudScale)
	g.initTerrainPatches()
	// Default camera: centred on battlefield, zoom 0.5 so the full map is visible.
	g.camX = float64(battleW) / 2
	g.camY = float64(battleH) / 2
	g.camZoom = 0.5
	g.simSpeed = 1.0
	g.speechRng = rand.New(rand.NewSource(time.Now().UnixNano() + 9999))
	g.reporter = NewSimReporter(reportWindowTicks, false)
	return g
}

// initTerrainPatches generates deterministic subtle ground colour patches.
func (g *Game) initTerrainPatches() {
	rng := rand.New(rand.NewSource(54321)) // #nosec G404 -- cosmetic only
	count := 600                           // more patches for the larger map
	g.terrainPatches = make([]terrainPatch, 0, count)
	for i := 0; i < count; i++ {
		w := float32(24 + rng.Intn(80))
		h := float32(24 + rng.Intn(80))
		x := float32(rng.Intn(g.gameWidth))
		y := float32(rng.Intn(g.gameHeight))
		// shade offset: -6 to +6 from base green
		shade := uint8(rng.Intn(13))
		g.terrainPatches = append(g.terrainPatches, terrainPatch{x: x, y: y, w: w, h: h, shade: shade})
	}
}

func (g *Game) initCover() {
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + 12345)) // #nosec G404 -- game only
	var rubble []*CoverObject
	g.covers, rubble = GenerateCover(g.gameWidth, g.gameHeight, g.buildingFootprints, g.buildings, rng, g.roads)
	// Rubble replaces wall segments where explosions hit — remove those walls and add rubble.
	g.applyBuildingDamage(rubble)
}

// applyBuildingDamage removes wall segments that overlap rubble zones and appends
// the rubble pieces to g.covers. This simulates pre-battle artillery damage:
// holes are blown in building walls and rubble scatters across the interior.
func (g *Game) applyBuildingDamage(rubble []*CoverObject) {
	cs := coverCellSize
	// Build a set of rubble cells for fast lookup.
	type cellKey struct{ cx, cy int }
	rubbleSet := make(map[cellKey]bool, len(rubble))
	for _, r := range rubble {
		rubbleSet[cellKey{r.x / cs, r.y / cs}] = true
	}
	// Remove wall segments that are covered by rubble.
	kept := g.buildings[:0]
	for _, w := range g.buildings {
		k := cellKey{w.x / cs, w.y / cs}
		if !rubbleSet[k] {
			kept = append(kept, w)
		}
	}
	g.buildings = kept
	// Add rubble to the cover list.
	g.covers = append(g.covers, rubble...)
}

func (g *Game) initBuildings() {
	rng := rand.New(rand.NewSource(time.Now().UnixNano())) // #nosec G404

	wall := cellSize // 16px
	unit := 64
	targetCount := 28 // more buildings on the larger map

	g.buildings = g.buildings[:0]
	g.buildingFootprints = g.buildingFootprints[:0]

	// Build a pool of candidates positioned along road edges.
	// Use varied sizes so the streetscape looks organic.
	sizeSets := [][2]int{
		{3, 3}, {3, 4}, {4, 3}, {4, 4}, {4, 5}, {5, 4}, {5, 5}, {5, 6}, {6, 5},
	}
	var candidates []rect
	for _, sz := range sizeSets {
		wUnits, hUnits := sz[0], sz[1]
		c := g.buildingCandidatesAlongRoads(rng, wUnits*unit, hUnits*unit, unit/2, unit*2)
		candidates = append(candidates, c...)
	}
	// Shuffle the combined pool.
	rng.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })

	for _, candidate := range candidates {
		if len(g.buildingFootprints) >= targetCount {
			break
		}
		if g.overlapsAny(candidate) {
			continue
		}
		if g.rectOverlapsRoad(candidate) {
			continue
		}
		g.buildingFootprints = append(g.buildingFootprints, candidate)
		g.addBuildingWalls(rng, candidate, wall, unit)
	}
}

// addBuildingWalls generates wall segments for a building footprint.
// It places perimeter walls with doorways and windows, plus optional internal partitions.
// Windows are wall-like segments that block movement but are transparent to LOS.
func (g *Game) addBuildingWalls(rng *rand.Rand, fp rect, wall, unit int) {
	x, y, w, h := fp.x, fp.y, fp.w, fp.h
	wUnits := w / unit
	hUnits := h / unit

	if wUnits < 3 {
		wUnits = 3
	}
	if hUnits < 3 {
		hUnits = 3
	}

	// Pick a doorway position per wall face (skip corners by using inner cells only).
	doorN := x + (rng.Intn(wUnits-2)+1)*unit
	doorS := x + (rng.Intn(wUnits-2)+1)*unit
	doorW := y + (rng.Intn(hUnits-2)+1)*unit
	doorE := y + (rng.Intn(hUnits-2)+1)*unit

	// Pick window positions: 1–2 per face, different from doorway.
	// Windows are unit-wide gaps in the wall that block movement but not LOS.
	pickWindow := func(faceStart, faceLen, doorPos int, count int) map[int]bool {
		wins := make(map[int]bool)
		for i := 0; i < count; i++ {
			for tries := 0; tries < 10; tries++ {
				innerUnits := faceLen/unit - 2
				if innerUnits < 1 {
					break
				}
				pos := faceStart + (rng.Intn(innerUnits)+1)*unit
				if pos == doorPos || wins[pos] {
					continue
				}
				wins[pos] = true
				break
			}
		}
		return wins
	}

	winCount := 1
	if wUnits >= 5 || hUnits >= 5 {
		winCount = 2
	}
	winN := pickWindow(x, w, doorN, winCount)
	winS := pickWindow(x, w, doorS, winCount)
	winW := pickWindow(y, h, doorW, winCount)
	winE := pickWindow(y, h, doorE, winCount)

	// isCornerCell returns true if the wall cell is in the first or last unit of the face.
	isCornerH := func(wx, faceX, faceW int) bool {
		return wx < faceX+unit || wx >= faceX+faceW-unit
	}
	isCornerV := func(wy, faceY, faceH int) bool {
		return wy < faceY+unit || wy >= faceY+faceH-unit
	}

	// North wall (top).
	for wx := x; wx < x+w; wx += wall {
		if wx >= doorN && wx < doorN+unit {
			continue
		}
		r := rect{x: wx, y: y, w: wall, h: wall}
		isWin := false
		for wpos := range winN {
			if wx >= wpos && wx < wpos+unit && !isCornerH(wx, x, w) {
				isWin = true
				break
			}
		}
		if isWin {
			g.windows = append(g.windows, r)
		} else {
			g.buildings = append(g.buildings, r)
		}
	}
	// South wall (bottom).
	for wx := x; wx < x+w; wx += wall {
		if wx >= doorS && wx < doorS+unit {
			continue
		}
		r := rect{x: wx, y: y + h - wall, w: wall, h: wall}
		isWin := false
		for wpos := range winS {
			if wx >= wpos && wx < wpos+unit && !isCornerH(wx, x, w) {
				isWin = true
				break
			}
		}
		if isWin {
			g.windows = append(g.windows, r)
		} else {
			g.buildings = append(g.buildings, r)
		}
	}
	// West wall (left), skip corners.
	for wy := y + wall; wy < y+h-wall; wy += wall {
		if wy >= doorW && wy < doorW+unit {
			continue
		}
		r := rect{x: x, y: wy, w: wall, h: wall}
		isWin := false
		for wpos := range winW {
			if wy >= wpos && wy < wpos+unit && !isCornerV(wy, y, h) {
				isWin = true
				break
			}
		}
		if isWin {
			g.windows = append(g.windows, r)
		} else {
			g.buildings = append(g.buildings, r)
		}
	}
	// East wall (right), skip corners.
	for wy := y + wall; wy < y+h-wall; wy += wall {
		if wy >= doorE && wy < doorE+unit {
			continue
		}
		r := rect{x: x + w - wall, y: wy, w: wall, h: wall}
		isWin := false
		for wpos := range winE {
			if wy >= wpos && wy < wpos+unit && !isCornerV(wy, y, h) {
				isWin = true
				break
			}
		}
		if isWin {
			g.windows = append(g.windows, r)
		} else {
			g.buildings = append(g.buildings, r)
		}
	}

	// Internal partition walls with doorways (no windows on internal walls).
	partitions := 1
	if wUnits >= 5 || hUnits >= 5 {
		partitions = 2
	}
	for p := 0; p < partitions; p++ {
		if rng.Intn(2) == 0 && wUnits >= 4 {
			px := x + (rng.Intn(wUnits-2)+1)*unit
			doorY := y + (rng.Intn(hUnits-2)+1)*unit
			for wy := y + wall; wy < y+h-wall; wy += wall {
				if wy >= doorY && wy < doorY+unit {
					continue
				}
				g.buildings = append(g.buildings, rect{x: px, y: wy, w: wall, h: wall})
			}
		} else if hUnits >= 4 {
			py := y + (rng.Intn(hUnits-2)+1)*unit
			doorX := x + (rng.Intn(wUnits-2)+1)*unit
			for wx := x + wall; wx < x+w-wall; wx += wall {
				if wx >= doorX && wx < doorX+unit {
					continue
				}
				g.buildings = append(g.buildings, rect{x: wx, y: py, w: wall, h: wall})
			}
		}
	}
}

func (g *Game) overlapsAny(r rect) bool {
	pad := 16
	rx0 := r.x - pad
	ry0 := r.y - pad
	rx1 := r.x + r.w + pad
	ry1 := r.y + r.h + pad

	for _, b := range g.buildingFootprints {
		if rx0 < b.x+b.w && rx1 > b.x && ry0 < b.y+b.h && ry1 > b.y {
			return true
		}
	}
	// Also reject if the candidate overlaps any road.
	for i := range g.roads {
		rd := &g.roads[i]
		if rx0 < rd.x+rd.w && rx1 > rd.x && ry0 < rd.y+rd.h && ry1 > rd.y {
			return true
		}
	}
	return false
}

// spawnCluster creates squadSize soldiers of the given team at startX,
// spread vertically around clusterCenterY. Returns the created soldiers.
func (g *Game) spawnCluster(rng *rand.Rand, team Team, squadSize int, clusterCenterY, startX, endX float64) []*Soldier {
	margin := 64.0
	spacing := 18.0 // tighter — squad spawns as a compact cluster, not a long line
	out := make([]*Soldier, 0, squadSize)
	sy := clusterCenterY - spacing*float64(squadSize-1)/2
	for i := 0; i < squadSize; i++ {
		jitter := (rng.Float64() - 0.5) * 3.0 // reduced jitter
		y := sy + float64(i)*spacing + jitter
		if y < margin {
			y = margin
		}
		if y > float64(g.gameHeight)-margin {
			y = float64(g.gameHeight) - margin
		}
		id := g.nextID
		g.nextID++
		s := NewSoldier(id, startX, y, team,
			[2]float64{startX, y}, [2]float64{endX, y},
			g.navGrid, g.covers, g.buildings, g.thoughtLog, &g.tick, g.tacticalMap)
		s.buildingFootprints = g.buildingFootprints
		s.blackboard.ClaimedBuildingIdx = -1
		if s.path != nil {
			out = append(out, s)
		}
	}
	return out
}

func (g *Game) initSoldiers() {
	rng := rand.New(rand.NewSource(time.Now().UnixNano())) // #nosec G404
	sqSz := 8
	margin := 64.0
	startX := margin
	endX := float64(g.gameWidth) - margin
	g.soldiers = append(g.soldiers, g.spawnCluster(rng, TeamRed, sqSz, float64(g.gameHeight)*0.28, startX, endX)...)
	g.soldiers = append(g.soldiers, g.spawnCluster(rng, TeamRed, sqSz, float64(g.gameHeight)*0.72, startX, endX)...)
}

func (g *Game) initOpFor() {
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + 999)) // #nosec G404
	sqSz := 8
	margin := 64.0
	startX := float64(g.gameWidth) - margin
	endX := margin
	g.opfor = append(g.opfor, g.spawnCluster(rng, TeamBlue, sqSz, float64(g.gameHeight)*0.28, startX, endX)...)
	g.opfor = append(g.opfor, g.spawnCluster(rng, TeamBlue, sqSz, float64(g.gameHeight)*0.72, startX, endX)...)
}

func (g *Game) initSquads() {
	sqSz := 8
	for i := 0; i < len(g.soldiers); i += sqSz {
		end := i + sqSz
		if end > len(g.soldiers) {
			end = len(g.soldiers)
		}
		sq := NewSquad(len(g.squads), TeamRed, g.soldiers[i:end])
		sq.buildingFootprints = g.buildingFootprints
		g.squads = append(g.squads, sq)
	}
	for i := 0; i < len(g.opfor); i += sqSz {
		end := i + sqSz
		if end > len(g.opfor) {
			end = len(g.opfor)
		}
		sq := NewSquad(len(g.squads), TeamBlue, g.opfor[i:end])
		sq.buildingFootprints = g.buildingFootprints
		g.squads = append(g.squads, sq)
	}
}

// randomiseProfiles gives each soldier slightly different stats so behaviour varies.
func (g *Game) randomiseProfiles() {
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + 42)) // #nosec G404 -- game only, crypto/rand not needed
	all := append(g.soldiers[:len(g.soldiers):len(g.soldiers)], g.opfor...)
	for _, s := range all {
		p := &s.profile
		p.Physical.FitnessBase = 0.4 + rng.Float64()*0.5 // 0.4 - 0.9
		p.Skills.Marksmanship = 0.2 + rng.Float64()*0.6  // 0.2 - 0.8
		p.Skills.Fieldcraft = 0.2 + rng.Float64()*0.6
		p.Skills.Discipline = 0.3 + rng.Float64()*0.6 // 0.3 - 0.9
		p.Psych.Experience = rng.Float64() * 0.5      // 0.0 - 0.5
		p.Psych.Morale = 0.5 + rng.Float64()*0.4      // 0.5 - 0.9
		p.Psych.Composure = 0.3 + rng.Float64()*0.5   // 0.3 - 0.8

		// Initialise commitment-based decision thresholds from discipline.
		s.blackboard.InitCommitment(p.Skills.Discipline)
	}
}

func (g *Game) Update() error {
	// Handle input every frame regardless of sim speed.
	g.handleInput()

	if g.simSpeed <= 0 {
		// Paused: still update tracers so muzzle flashes fade.
		g.combat.UpdateTracers()
		return nil
	}

	// For speeds > 1 run multiple sim ticks per frame.
	// For speeds < 1 accumulate fractions.
	g.tickAccum += g.simSpeed
	for g.tickAccum >= 1.0 {
		g.tickAccum -= 1.0
		g.simTick()
	}
	return nil
}

// simTick runs one simulation tick.
func (g *Game) simTick() {
	g.tick++

	// 1. SENSE: each soldier scans for enemies.
	for _, s := range g.soldiers {
		s.UpdateVision(g.opfor, g.buildings)
	}
	for _, s := range g.opfor {
		s.UpdateVision(g.soldiers, g.buildings)
	}

	// 2. COMBAT: fire decisions and resolution.
	all := append(g.soldiers[:len(g.soldiers):len(g.soldiers)], g.opfor...)
	g.combat.ResetFireCounts(all)
	g.combat.ResolveCombat(g.soldiers, g.opfor, g.soldiers, g.buildings, all)
	g.combat.ResolveCombat(g.opfor, g.soldiers, g.opfor, g.buildings, all)
	g.combat.UpdateTracers()

	// 2.1. SOUND: broadcast gunfire events to enemy soldiers (infinite range).
	g.combat.BroadcastGunfire(g.soldiers, g.opfor, g.tick)

	// 2.5. INTEL: update all heatmap layers from current soldier state.
	g.intel.Update(g.soldiers, g.opfor, g.buildings)

	// 3. SQUAD THINK: leaders evaluate and set intent/orders.
	for _, sq := range g.squads {
		sq.SquadThink(g.intel)
	}

	// Formation pass: update slot targets before soldiers decide to move.
	for _, sq := range g.squads {
		sq.UpdateFormation()
	}

	// 4+5. INDIVIDUAL THINK + ACT.
	for _, s := range g.soldiers {
		s.Update()
	}
	for _, s := range g.opfor {
		s.Update()
	}

	// 6. SQUAD POLL: periodic summary to thought log (~every 5s).
	if g.tick%squadPollInterval == 0 {
		for _, sq := range g.squads {
			g.thoughtLog.AddSquadPoll(g.tick, sq)
		}
	}

	// 7. SPEECH: update soldier speech bubbles.
	g.UpdateSpeech(g.speechRng)

	// 8. ANALYTICS: collect behaviour report every ~1s.
	if g.tick%60 == 0 && g.reporter != nil {
		g.reporter.Collect(g.tick, g.soldiers, g.opfor, g.squads)
	}
}

// handleInput processes overlay toggle keypresses (edge-triggered).
func (g *Game) handleInput() {
	currentKeys := map[ebiten.Key]bool{}

	// Layer toggles for active team: 1-6.
	layerKeys := [intelMapCount]ebiten.Key{
		ebiten.Key1, ebiten.Key2, ebiten.Key3,
		ebiten.Key4, ebiten.Key5, ebiten.Key6,
	}
	for i, k := range layerKeys {
		currentKeys[k] = ebiten.IsKeyPressed(k)
		if currentKeys[k] && !g.prevKeys[k] {
			g.showOverlay[g.overlayTeam][i] = !g.showOverlay[g.overlayTeam][i]
		}
	}

	// Tab: switch which team's maps are displayed.
	currentKeys[ebiten.KeyTab] = ebiten.IsKeyPressed(ebiten.KeyTab)
	if currentKeys[ebiten.KeyTab] && !g.prevKeys[ebiten.KeyTab] {
		g.overlayTeam = 1 - g.overlayTeam
	}

	// H: toggle HUD key legend.
	currentKeys[ebiten.KeyH] = ebiten.IsKeyPressed(ebiten.KeyH)
	if currentKeys[ebiten.KeyH] && !g.prevKeys[ebiten.KeyH] {
		g.showHUD = !g.showHUD
	}

	// Camera pan: WASD or arrow keys.
	panSpeed := 6.0 / g.camZoom // pan slower when zoomed in
	if ebiten.IsKeyPressed(ebiten.KeyW) || ebiten.IsKeyPressed(ebiten.KeyArrowUp) {
		g.camY -= panSpeed
	}
	if ebiten.IsKeyPressed(ebiten.KeyS) || ebiten.IsKeyPressed(ebiten.KeyArrowDown) {
		g.camY += panSpeed
	}
	if ebiten.IsKeyPressed(ebiten.KeyA) || ebiten.IsKeyPressed(ebiten.KeyArrowLeft) {
		g.camX -= panSpeed
	}
	if ebiten.IsKeyPressed(ebiten.KeyD) || ebiten.IsKeyPressed(ebiten.KeyArrowRight) {
		g.camX += panSpeed
	}

	// Camera zoom: mouse wheel or =/- keys.
	const zoomMin, zoomMax = 0.5, 4.0
	_, wy := ebiten.Wheel()
	if wy != 0 {
		g.camZoom *= math.Pow(1.12, wy)
	}
	currentKeys[ebiten.KeyEqual] = ebiten.IsKeyPressed(ebiten.KeyEqual)
	if currentKeys[ebiten.KeyEqual] && !g.prevKeys[ebiten.KeyEqual] {
		g.camZoom *= 1.25
	}
	currentKeys[ebiten.KeyMinus] = ebiten.IsKeyPressed(ebiten.KeyMinus)
	if currentKeys[ebiten.KeyMinus] && !g.prevKeys[ebiten.KeyMinus] {
		g.camZoom /= 1.25
	}
	if g.camZoom < zoomMin {
		g.camZoom = zoomMin
	}
	if g.camZoom > zoomMax {
		g.camZoom = zoomMax
	}

	// Clamp camera centre to battlefield bounds (accounting for zoom).
	halfVW := float64(g.gameWidth) / 2 / g.camZoom
	halfVH := float64(g.gameHeight) / 2 / g.camZoom
	if g.camX < halfVW {
		g.camX = halfVW
	}
	if g.camX > float64(g.gameWidth)-halfVW {
		g.camX = float64(g.gameWidth) - halfVW
	}
	if g.camY < halfVH {
		g.camY = halfVH
	}
	if g.camY > float64(g.gameHeight)-halfVH {
		g.camY = float64(g.gameHeight) - halfVH
	}

	// Sim speed controls: P=pause/resume, ,=slower, .=faster.
	speeds := []float64{0, 0.5, 1, 2, 4}
	currentKeys[ebiten.KeyP] = ebiten.IsKeyPressed(ebiten.KeyP)
	if currentKeys[ebiten.KeyP] && !g.prevKeys[ebiten.KeyP] {
		if g.simSpeed > 0 {
			g.simSpeed = 0
		} else {
			g.simSpeed = 1
		}
	}
	currentKeys[ebiten.KeyComma] = ebiten.IsKeyPressed(ebiten.KeyComma)
	if currentKeys[ebiten.KeyComma] && !g.prevKeys[ebiten.KeyComma] {
		for i, s := range speeds {
			if s >= g.simSpeed && i > 0 {
				g.simSpeed = speeds[i-1]
				break
			}
		}
	}
	currentKeys[ebiten.KeyPeriod] = ebiten.IsKeyPressed(ebiten.KeyPeriod)
	if currentKeys[ebiten.KeyPeriod] && !g.prevKeys[ebiten.KeyPeriod] {
		for i, s := range speeds {
			if s <= g.simSpeed && i < len(speeds)-1 {
				if speeds[i+1] > g.simSpeed {
					g.simSpeed = speeds[i+1]
					break
				}
			}
		}
	}

	// Left mouse click: try to select a soldier.
	if ebiten.IsMouseButtonPressed(ebiten.MouseButtonLeft) {
		if !g.prevMouseLeft {
			mx, my := ebiten.CursorPosition()
			g.handleInspectorClick(mx, my)
		}
	}
	g.prevMouseLeft = ebiten.IsMouseButtonPressed(ebiten.MouseButtonLeft)

	// I: toggle inspector raw/curated view.
	currentKeys[ebiten.KeyI] = ebiten.IsKeyPressed(ebiten.KeyI)
	if currentKeys[ebiten.KeyI] && !g.prevKeys[ebiten.KeyI] {
		g.inspector.rawView = !g.inspector.rawView
	}

	// F: toggle fullscreen.
	currentKeys[ebiten.KeyF] = ebiten.IsKeyPressed(ebiten.KeyF)
	if currentKeys[ebiten.KeyF] && !g.prevKeys[ebiten.KeyF] {
		ebiten.SetFullscreen(!ebiten.IsFullscreen())
	}

	g.prevKeys = currentKeys
}

func (g *Game) Draw(screen *ebiten.Image) {
	// Window background: very dark, outside battlefield.
	screen.Fill(color.RGBA{R: 12, G: 14, B: 12, A: 255})

	// Render world content to worldBuf at (0,0) origin, then blit with camera transform.
	g.worldBuf.Clear()
	g.drawWorld(g.worldBuf)

	// Camera transform: translate so camX/camY is at viewport centre, then scale.
	vpW := float64(g.gameWidth)
	vpH := float64(g.gameHeight)
	var cam ebiten.GeoM
	cam.Translate(-g.camX, -g.camY)
	cam.Scale(g.camZoom, g.camZoom)
	cam.Translate(vpW/2, vpH/2)

	opt := &ebiten.DrawImageOptions{GeoM: cam}
	// Clip to the battlefield viewport.
	clipX := float32(g.offX)
	clipY := float32(g.offY)
	// Draw worldBuf onto screen at the battlefield offset.
	var blit ebiten.DrawImageOptions
	blit.GeoM = cam
	blit.GeoM.Translate(float64(g.offX), float64(g.offY))
	screen.DrawImage(g.worldBuf, &blit)
	_ = opt
	_ = clipX
	_ = clipY

	// Battlefield border frame (drawn at screen coords, not transformed).
	ox := float32(g.offX)
	oy := float32(g.offY)
	gw := float32(g.gameWidth)
	gh := float32(g.gameHeight)
	borderCol := color.RGBA{R: 65, G: 90, B: 65, A: 255}
	vector.StrokeRect(screen, ox-1, oy-1, gw+2, gh+2, 2.0, borderCol, false)
	vector.StrokeRect(screen, ox-3, oy-3, gw+6, gh+6, 1.0, color.RGBA{R: 40, G: 65, B: 40, A: 100}, false)

	// Thought log panel (screen coords).
	logX := g.offX + g.gameWidth + g.offX
	g.thoughtLog.Draw(screen, logX, g.height)

	// HUD key legend.
	if g.showHUD {
		g.drawHUD(screen)
	}

	// Zoom indicator.
	if g.camZoom != 1.0 {
		ebitenutil.DebugPrintAt(screen, fmt.Sprintf("zoom: %.1fx", g.camZoom), g.offX+6, g.offY+6)
	}

	// Soldier inspector panel (screen-space, drawn over everything).
	g.drawInspector(screen)
}

func (g *Game) drawWorld(screen *ebiten.Image) {
	// For drawWorld, all coordinates are in world-space (no offX/offY needed).
	ox, oy := float32(0), float32(0)
	gw, gh := float32(g.gameWidth), float32(g.gameHeight)

	// Ground fill inside battlefield.
	vector.FillRect(screen, ox, oy, gw, gh, color.RGBA{R: 28, G: 42, B: 28, A: 255}, false)

	// Terrain noise patches — subtle colour variation on the ground.
	for _, tp := range g.terrainPatches {
		baseG := int(42) + int(tp.shade) - 6
		if baseG < 0 {
			baseG = 0
		}
		if baseG > 255 {
			baseG = 255
		}
		baseR := 28 + int(tp.shade)/2 - 3
		baseB := 28 + int(tp.shade)/3 - 2
		if baseR < 0 {
			baseR = 0
		}
		if baseB < 0 {
			baseB = 0
		}
		vector.FillRect(screen, ox+tp.x, oy+tp.y, tp.w, tp.h,
			color.RGBA{R: uint8(baseR), G: uint8(baseG), B: uint8(baseB), A: 40}, false)
	}

	gridFine := 16
	gridMid := gridFine * 4
	gridCoarse := gridMid * 4

	drawGridOffset(screen, 0, 0, g.gameWidth, g.gameHeight, gridFine, color.RGBA{R: 32, G: 47, B: 32, A: 255})
	drawGridOffset(screen, 0, 0, g.gameWidth, g.gameHeight, gridMid, color.RGBA{R: 38, G: 55, B: 38, A: 255})
	drawGridOffset(screen, 0, 0, g.gameWidth, g.gameHeight, gridCoarse, color.RGBA{R: 48, G: 68, B: 48, A: 255})

	// Roads — drawn before buildings so buildings sit on top of the road surface.
	roadFill := color.RGBA{R: 48, G: 46, B: 42, A: 255} // dark asphalt
	roadEdge := color.RGBA{R: 62, G: 60, B: 54, A: 255} // slightly lighter kerb
	roadMark := color.RGBA{R: 70, G: 68, B: 58, A: 120} // faint centre-line
	for _, rd := range g.roads {
		rx := ox + float32(rd.x)
		ry := oy + float32(rd.y)
		rw := float32(rd.w)
		rh := float32(rd.h)
		vector.FillRect(screen, rx, ry, rw, rh, roadFill, false)
		// Edge lines.
		vector.StrokeLine(screen, rx, ry, rx+rw, ry, 1.0, roadEdge, false)
		vector.StrokeLine(screen, rx, ry+rh, rx+rw, ry+rh, 1.0, roadEdge, false)
		vector.StrokeLine(screen, rx, ry, rx, ry+rh, 1.0, roadEdge, false)
		vector.StrokeLine(screen, rx+rw, ry, rx+rw, ry+rh, 1.0, roadEdge, false)
		// Dashed centre marking.
		if rd.horiz {
			cy := ry + rh/2
			dashLen := float32(24)
			gap := float32(16)
			for x := rx; x < rx+rw; x += dashLen + gap {
				end := x + dashLen
				if end > rx+rw {
					end = rx + rw
				}
				vector.StrokeLine(screen, x, cy, end, cy, 1.0, roadMark, false)
			}
		} else {
			cx := rx + rw/2
			dashLen := float32(24)
			gap := float32(16)
			for y := ry; y < ry+rh; y += dashLen + gap {
				end := y + dashLen
				if end > ry+rh {
					end = ry + rh
				}
				vector.StrokeLine(screen, cx, y, cx, end, 1.0, roadMark, false)
			}
		}
	}

	// Intel heatmap overlays (drawn under buildings and soldiers).
	team := Team(g.overlayTeam)
	if im := g.intel.For(team); im != nil {
		for k := IntelMapKind(0); k < intelMapCount; k++ {
			if g.showOverlay[g.overlayTeam][k] {
				g.drawHeatLayer(screen, im.Layer(k), overlayColors[k])
			}
		}
	}

	// Build a set of claimed building indices → team for tinting.
	claimedTeam := make(map[int]Team)
	for _, sq := range g.squads {
		if sq.ClaimedBuildingIdx >= 0 {
			claimedTeam[sq.ClaimedBuildingIdx] = sq.Team
		}
	}

	// Building interiors: floor first, then walls on top.
	// Floor areas (from footprints) — dark interior.
	for i, b := range g.buildingFootprints {
		x0 := ox + float32(b.x)
		y0 := oy + float32(b.y)
		bw := float32(b.w)
		bh := float32(b.h)

		// Soft drop shadow.
		vector.FillRect(screen, x0+4, y0+4, bw, bh, color.RGBA{R: 10, G: 8, B: 6, A: 80}, false)
		// Floor fill — dark concrete.
		vector.FillRect(screen, x0, y0, bw, bh, color.RGBA{R: 38, G: 36, B: 32, A: 255}, false)

		// Team tint overlay for claimed buildings.
		if t, ok := claimedTeam[i]; ok {
			var tint color.RGBA
			if t == TeamRed {
				tint = color.RGBA{R: 140, G: 30, B: 20, A: 35}
			} else {
				tint = color.RGBA{R: 20, G: 40, B: 140, A: 35}
			}
			vector.FillRect(screen, x0, y0, bw, bh, tint, false)
			// Thin team-colour border.
			var border color.RGBA
			if t == TeamRed {
				border = color.RGBA{R: 200, G: 60, B: 40, A: 80}
			} else {
				border = color.RGBA{R: 40, G: 80, B: 200, A: 80}
			}
			vector.StrokeRect(screen, x0, y0, bw, bh, 1.0, border, false)
		}

		// Subtle tile grid on the floor.
		tileCol := color.RGBA{R: 46, G: 44, B: 39, A: 180}
		tileStep := float32(cellSize)
		for tx := x0 + tileStep; tx < x0+bw; tx += tileStep {
			vector.StrokeLine(screen, tx, y0, tx, y0+bh, 0.5, tileCol, false)
		}
		for ty := y0 + tileStep; ty < y0+bh; ty += tileStep {
			vector.StrokeLine(screen, x0, ty, x0+bw, ty, 0.5, tileCol, false)
		}
	}
	// Wall segments — drawn as solid filled cells with lighting.
	wallFill := color.RGBA{R: 85, G: 80, B: 68, A: 255}
	wallLight := color.RGBA{R: 110, G: 105, B: 90, A: 200}
	wallDark := color.RGBA{R: 50, G: 47, B: 38, A: 200}
	for _, b := range g.buildings {
		x0 := ox + float32(b.x)
		y0 := oy + float32(b.y)
		bw := float32(b.w)
		bh := float32(b.h)
		vector.FillRect(screen, x0, y0, bw, bh, wallFill, false)
		// Top-left highlight.
		vector.StrokeLine(screen, x0, y0, x0+bw, y0, 0.5, wallLight, false)
		vector.StrokeLine(screen, x0, y0, x0, y0+bh, 0.5, wallLight, false)
		// Bottom-right shadow.
		vector.StrokeLine(screen, x0, y0+bh, x0+bw, y0+bh, 0.5, wallDark, false)
		vector.StrokeLine(screen, x0+bw, y0, x0+bw, y0+bh, 0.5, wallDark, false)
	}

	// Window segments — translucent blue glass over the floor.
	for _, w := range g.windows {
		x0 := ox + float32(w.x)
		y0 := oy + float32(w.y)
		bw := float32(w.w)
		bh := float32(w.h)
		// Glass fill — dark blue-tinted, semi-transparent.
		vector.FillRect(screen, x0, y0, bw, bh, color.RGBA{R: 55, G: 70, B: 100, A: 180}, false)
		// Bright highlight edge (top-left) to suggest reflective glass.
		vector.StrokeLine(screen, x0, y0, x0+bw, y0, 0.5, color.RGBA{R: 120, G: 160, B: 210, A: 220}, false)
		vector.StrokeLine(screen, x0, y0, x0, y0+bh, 0.5, color.RGBA{R: 110, G: 150, B: 200, A: 180}, false)
	}

	// Cover objects.
	g.drawCoverObjects(screen, 0, 0)

	// Vision cones (rendered to offscreen buffer then composited; world-space).
	g.drawVisionConesBuffered(screen, g.soldiers, color.RGBA{R: 180, G: 40, B: 30, A: 255}, 0.06)
	g.drawVisionConesBuffered(screen, g.opfor, color.RGBA{R: 30, G: 60, B: 180, A: 255}, 0.06)

	for _, s := range g.soldiers {
		s.Draw(screen, 0, 0)
	}
	for _, s := range g.opfor {
		s.Draw(screen, 0, 0)
	}

	// Movement intent lines: faint dashed line from soldier to path endpoint.
	g.drawMovementIntentLines(screen)

	// Squad intent labels near leaders.
	g.drawSquadIntentLabels(screen)

	// Selection ring for inspector target (world-space).
	if g.inspector.selected != nil {
		sel := g.inspector.selected
		if sel.state != SoldierStateDead {
			sr := float32(soldierRadius + 5)
			sx := float32(sel.x)
			sy := float32(sel.y)
			ringCol := color.RGBA{R: 255, G: 240, B: 60, A: 220}
			for a := 0; a < 16; a++ {
				ang0 := float64(a) / 16.0 * 2 * math.Pi
				ang1 := float64(a+1) / 16.0 * 2 * math.Pi
				vector.StrokeLine(screen,
					sx+sr*float32(math.Cos(ang0)),
					sy+sr*float32(math.Sin(ang0)),
					sx+sr*float32(math.Cos(ang1)),
					sy+sr*float32(math.Sin(ang1)),
					1.5, ringCol, false)
			}
		}
	}

	// Dynamic gunfire light blooms — drawn before muzzle flashes so they sit underneath.
	g.drawGunfireLighting(screen, 0, 0)

	// Muzzle flashes and tracers.
	g.combat.DrawMuzzleFlashes(screen, 0, 0)
	g.combat.DrawTracers(screen, 0, 0)

	// Speech bubbles above soldiers.
	g.drawSpeechBubbles(screen, 0, 0)

	// Detailed info panel for selected soldier (world-space).
	g.drawSelectedSoldierInfo(screen)

	// Spotted indicator.
	g.drawSpottedIndicators(screen, 0, 0)

	// Edge vignette — deeper for more atmosphere.
	g.drawVignette(screen, 0, 0)
}

// drawCoverObjects renders cover objects with orientation-aware visuals.
// Chest-walls detect their H/V neighbours to draw correct cross-sections and corners.
func (g *Game) drawCoverObjects(screen *ebiten.Image, offX, offY int) {
	ox, oy := float32(offX), float32(offY)
	cs := float32(coverCellSize) // 16px
	iCS := coverCellSize

	// Build a fast set of chest-wall cell positions for neighbour lookup.
	type cellKey struct{ cx, cy int }
	chestSet := map[cellKey]bool{}
	for _, c := range g.covers {
		if c.kind == CoverChestWall {
			chestSet[cellKey{c.x / iCS, c.y / iCS}] = true
		}
	}

	// Palette.
	slabBody := color.RGBA{R: 100, G: 94, B: 80, A: 255}
	slabTop := color.RGBA{R: 138, G: 130, B: 112, A: 255} // bright top-face highlight
	slabSide := color.RGBA{R: 72, G: 68, B: 56, A: 255}   // darker side face
	slabShadow := color.RGBA{R: 8, G: 6, B: 4, A: 90}
	slabW := float32(5) // thickness of the wall strip in pixels

	for _, c := range g.covers {
		x0 := ox + float32(c.x)
		y0 := oy + float32(c.y)

		switch c.kind {
		case CoverTallWall:
			vector.FillRect(screen, x0+2, y0+2, cs, cs, color.RGBA{R: 8, G: 6, B: 4, A: 120}, false)
			vector.FillRect(screen, x0, y0, cs, cs, color.RGBA{R: 88, G: 82, B: 70, A: 255}, false)
			vector.StrokeLine(screen, x0, y0, x0+cs, y0, 1.0, color.RGBA{R: 118, G: 112, B: 96, A: 220}, false)
			vector.StrokeLine(screen, x0, y0, x0, y0+cs, 1.0, color.RGBA{R: 108, G: 102, B: 88, A: 180}, false)
			vector.StrokeLine(screen, x0, y0+cs, x0+cs, y0+cs, 1.0, color.RGBA{R: 45, G: 42, B: 34, A: 200}, false)

		case CoverChestWall:
			// Determine which neighbours exist.
			cx := c.x / iCS
			cy := c.y / iCS
			hasLeft := chestSet[cellKey{cx - 1, cy}]
			hasRight := chestSet[cellKey{cx + 1, cy}]
			hasUp := chestSet[cellKey{cx, cy - 1}]
			hasDown := chestSet[cellKey{cx, cy + 1}]
			hasH := hasLeft || hasRight
			hasV := hasUp || hasDown

			// Drop shadow first.
			vector.FillRect(screen, x0+2, y0+2, cs, cs, slabShadow, false)

			// Horizontal run: slab centred vertically across the cell (3D top-down wall).
			// The "top" of the wall is a bright horizontal stripe across the centre.
			if hasH && !hasV {
				// Pure horizontal — draw a horizontal slab.
				mid := y0 + cs/2 - slabW/2
				vector.FillRect(screen, x0, mid, cs, slabW, slabBody, false)
				vector.StrokeLine(screen, x0, mid, x0+cs, mid, 1.5, slabTop, false)
				vector.StrokeLine(screen, x0, mid+slabW, x0+cs, mid+slabW, 0.8, slabSide, false)
				// End caps only where wall ends.
				if !hasLeft {
					vector.StrokeLine(screen, x0, mid, x0, mid+slabW, 1.0, slabTop, false)
				}
				if !hasRight {
					vector.StrokeLine(screen, x0+cs, mid, x0+cs, mid+slabW, 1.0, slabSide, false)
				}
			} else if hasV && !hasH {
				// Pure vertical — draw a vertical slab.
				mid := x0 + cs/2 - slabW/2
				vector.FillRect(screen, mid, y0, slabW, cs, slabBody, false)
				vector.StrokeLine(screen, mid, y0, mid, y0+cs, 1.5, slabTop, false)
				vector.StrokeLine(screen, mid+slabW, y0, mid+slabW, y0+cs, 0.8, slabSide, false)
				if !hasUp {
					vector.StrokeLine(screen, mid, y0, mid+slabW, y0, 1.0, slabTop, false)
				}
				if !hasDown {
					vector.StrokeLine(screen, mid, y0+cs, mid+slabW, y0+cs, 1.0, slabSide, false)
				}
			} else {
				// Corner / T-junction / cross — draw both H and V slabs overlapping.
				// Horizontal band.
				midY := y0 + cs/2 - slabW/2
				vector.FillRect(screen, x0, midY, cs, slabW, slabBody, false)
				// Vertical band.
				midX := x0 + cs/2 - slabW/2
				vector.FillRect(screen, midX, y0, slabW, cs, slabBody, false)
				// Top-face highlights along both axes.
				vector.StrokeLine(screen, x0, midY, x0+cs, midY, 1.5, slabTop, false)
				vector.StrokeLine(screen, midX, y0, midX, y0+cs, 1.5, slabTop, false)
				vector.StrokeLine(screen, x0, midY+slabW, x0+cs, midY+slabW, 0.8, slabSide, false)
				vector.StrokeLine(screen, midX+slabW, y0, midX+slabW, y0+cs, 0.8, slabSide, false)
			}

		case CoverRubble:
			vector.FillRect(screen, x0, y0, cs, cs, color.RGBA{R: 42, G: 38, B: 30, A: 160}, false)
			vector.FillRect(screen, x0+1, y0+cs-7, 7, 5, color.RGBA{R: 92, G: 84, B: 68, A: 240}, false)
			vector.StrokeLine(screen, x0+1, y0+cs-7, x0+8, y0+cs-7, 0.5, color.RGBA{R: 118, G: 110, B: 90, A: 200}, false)
			vector.FillRect(screen, x0+cs-7, y0+2, 5, 4, color.RGBA{R: 82, G: 76, B: 62, A: 230}, false)
			vector.FillRect(screen, x0+5, y0+5, 3, 3, color.RGBA{R: 100, G: 92, B: 76, A: 210}, false)
			vector.FillRect(screen, x0+3, y0+cs-4, 2, 2, color.RGBA{R: 60, G: 55, B: 44, A: 180}, false)
			vector.FillRect(screen, x0+cs-5, y0+cs-5, 2, 2, color.RGBA{R: 60, G: 55, B: 44, A: 160}, false)
			vector.FillRect(screen, x0+8, y0+10, 2, 2, color.RGBA{R: 72, G: 66, B: 54, A: 150}, false)
		}
	}
}

// drawHeatLayer renders one HeatLayer as an alpha-blended colour wash.
func (g *Game) drawHeatLayer(screen *ebiten.Image, layer *HeatLayer, baseCol color.RGBA) {
	ox, oy := float32(0), float32(0)
	cs := float32(cellSize)
	for row := 0; row < layer.rows; row++ {
		for col := 0; col < layer.cols; col++ {
			v := layer.At(row, col)
			if v < 0.01 {
				continue
			}
			alpha := uint8(float32(baseCol.A) * v)
			if alpha < 2 {
				continue
			}
			c := color.RGBA{
				R: baseCol.R,
				G: baseCol.G,
				B: baseCol.B,
				A: alpha,
			}
			vector.FillRect(screen,
				ox+float32(col)*cs, oy+float32(row)*cs,
				cs, cs, c, false)
		}
	}
}

// drawHUD renders keyboard shortcut hints in the bottom-left corner.
// Text is drawn into hudBuf at 1x then composited onto the screen at hudScale (3×).
func (g *Game) drawHUD(screen *ebiten.Image) {
	teamLabel := "RED"
	if g.overlayTeam == 1 {
		teamLabel = "BLUE"
	}

	speedStr := "1x"
	if g.simSpeed == 0 {
		speedStr = "PAUSED"
	} else if g.simSpeed == 2 {
		speedStr = "2x"
	} else if g.simSpeed == 4 {
		speedStr = "4x"
	} else if g.simSpeed != 1 {
		speedStr = fmt.Sprintf("%.1fx", g.simSpeed)
	}

	lines := []string{
		fmt.Sprintf("SIM: %s  P=pause  ,/. speed", speedStr),
		fmt.Sprintf("Intel: [%s]  Tab=switch", teamLabel),
	}
	for k := IntelMapKind(0); k < intelMapCount; k++ {
		on := " "
		if g.showOverlay[g.overlayTeam][k] {
			on = "*"
		}
		lines = append(lines, fmt.Sprintf("  [%d]%s %s", k+1, on, IntelMapKindName(k)))
	}
	lines = append(lines, "[F] fullscreen  [H] toggle HUD")
	lines = append(lines, "WASD/arrows=pan  scroll=zoom")
	lines = append(lines, fmt.Sprintf("zoom: %.1fx  click=inspect", g.camZoom))

	// Render into hudBuf at 1x, then scale up.
	const lineH = 12 // debug font line height at 1x
	const charW = 6  // debug font char width at 1x
	const padX = 5
	const padY = 4

	maxLen := 0
	for _, l := range lines {
		if len(l) > maxLen {
			maxLen = len(l)
		}
	}
	boxW := float32(maxLen*charW + padX*2)
	boxH := float32(len(lines)*lineH + padY*2)

	// Position in unscaled coordinates (hudBuf is screen/hudScale).
	bufW := float32(g.width / hudScale)
	bufH := float32(g.height / hudScale)
	bx := float32(4)
	by := bufH - boxH - 4

	g.hudBuf.Clear()
	// Panel background.
	vector.FillRect(g.hudBuf, bx, by, boxW, boxH,
		color.RGBA{R: 6, G: 10, B: 6, A: 210}, false)
	vector.StrokeRect(g.hudBuf, bx, by, boxW, boxH,
		1.0, color.RGBA{R: 60, G: 100, B: 60, A: 180}, false)
	// Inner highlight line along top edge.
	vector.StrokeLine(g.hudBuf, bx+1, by+1, bx+boxW-1, by+1,
		1.0, color.RGBA{R: 80, G: 140, B: 80, A: 80}, false)
	_ = bufW

	for i, line := range lines {
		tx := int(bx) + padX
		ty := int(by) + padY + i*lineH
		ebitenutil.DebugPrintAt(g.hudBuf, line, tx, ty)
	}

	// Blit hudBuf onto screen at hudScale.
	opts := &ebiten.DrawImageOptions{}
	opts.GeoM.Scale(float64(hudScale), float64(hudScale))
	screen.DrawImage(g.hudBuf, opts)
}

// drawFormationSlots renders a small ghost circle at each member's current slot target.
func (g *Game) drawFormationSlots(screen *ebiten.Image) {
	ox, oy := float32(g.offX), float32(g.offY)
	for _, sq := range g.squads {
		if sq.Leader == nil || sq.Leader.state == SoldierStateDead {
			continue
		}
		offsets := formationOffsets(sq.Formation, len(sq.Members))
		for i, m := range sq.Members {
			if i == 0 || !m.formationMember {
				continue
			}
			off := offsets[i]
			wx, wy := SlotWorld(sq.Leader.x, sq.Leader.y, sq.smoothedHeading, off[0], off[1])
			// Faint diamond: four short lines.
			d := float32(4.0)
			var c color.RGBA
			if m.team == TeamRed {
				c = color.RGBA{R: 220, G: 60, B: 60, A: 60}
			} else {
				c = color.RGBA{R: 60, G: 100, B: 220, A: 60}
			}
			swx, swy := ox+float32(wx), oy+float32(wy)
			vector.StrokeLine(screen, swx-d, swy, swx, swy-d, 1.0, c, false)
			vector.StrokeLine(screen, swx, swy-d, swx+d, swy, 1.0, c, false)
			vector.StrokeLine(screen, swx+d, swy, swx, swy+d, 1.0, c, false)
			vector.StrokeLine(screen, swx, swy+d, swx-d, swy, 1.0, c, false)
			// Line from member to their slot.
			vector.StrokeLine(screen, ox+float32(m.x), oy+float32(m.y), swx, swy, 1.0, color.RGBA{R: 255, G: 255, B: 255, A: 18}, false)
		}
	}
}

// drawSpottedIndicators renders a subtle "!" above soldiers who currently see enemies.
func (g *Game) drawSpottedIndicators(screen *ebiten.Image, offX, offY int) {
	ox, oy := float32(offX), float32(offY)
	all := append(g.soldiers[:len(g.soldiers):len(g.soldiers)], g.opfor...)
	for _, s := range all {
		if s.state == SoldierStateDead || len(s.vision.KnownContacts) == 0 {
			continue
		}
		sx, sy := ox+float32(s.x), oy+float32(s.y)
		// "!" drawn as a short vertical stroke + dot, offset above the soldier.
		var c color.RGBA
		if s.team == TeamRed {
			c = color.RGBA{R: 255, G: 200, B: 100, A: 160}
		} else {
			c = color.RGBA{R: 100, G: 200, B: 255, A: 160}
		}
		topY := sy - float32(soldierRadius) - 10
		// Stroke of the "!"
		vector.StrokeLine(screen, sx, topY, sx, topY+5, 1.5, c, false)
		// Dot of the "!"
		vector.FillCircle(screen, sx, topY+7, 1.0, c, false)
	}
}

// drawVisionConesBuffered renders all FOV fans for a team into an offscreen buffer,
// then composites that buffer onto the main screen with a single controlled opacity.
// This eliminates additive blowout from overlapping cones.
func (g *Game) drawVisionConesBuffered(screen *ebiten.Image, soldiers []*Soldier, tint color.RGBA, opacity float64) {
	buf := g.visionBuf
	buf.Clear()

	// Draw solid white fans into the buffer; we'll tint + fade on composite.
	for _, s := range soldiers {
		if s.state == SoldierStateDead {
			continue
		}
		v := &s.vision
		halfFOV := v.FOV / 2.0
		coneLen := v.MaxRange * 0.45
		if coneLen < 60 {
			coneLen = 60
		}
		const steps = 36
		sx, sy := float32(s.x), float32(s.y)

		var path vector.Path
		path.MoveTo(sx, sy)
		for i := 0; i <= steps; i++ {
			a := s.vision.Heading - halfFOV + (v.FOV/float64(steps))*float64(i)
			pxW, pyW := g.clipVisionRayToBuildings(s.x, s.y, a, coneLen)
			path.LineTo(float32(pxW), float32(pyW))
		}
		path.Close()

		vector.FillPath(buf, &path, &vector.FillOptions{}, &vector.DrawPathOptions{AntiAlias: true})

		// Faint outline on the buffer too.
		edgeCol := color.RGBA{R: 255, G: 255, B: 255, A: 80}
		// Bounding rays.
		a0 := s.vision.Heading - halfFOV
		a1 := s.vision.Heading + halfFOV
		p0x, p0y := g.clipVisionRayToBuildings(s.x, s.y, a0, coneLen)
		p1x, p1y := g.clipVisionRayToBuildings(s.x, s.y, a1, coneLen)
		vector.StrokeLine(buf, sx, sy, float32(p0x), float32(p0y), 1.0, edgeCol, false)
		vector.StrokeLine(buf, sx, sy, float32(p1x), float32(p1y), 1.0, edgeCol, false)
		// Arc edge.
		var prevPx, prevPy float32
		for i := 0; i <= steps; i++ {
			a := s.vision.Heading - halfFOV + (v.FOV/float64(steps))*float64(i)
			axW, ayW := g.clipVisionRayToBuildings(s.x, s.y, a, coneLen)
			px, py := float32(axW), float32(ayW)
			if i > 0 {
				vector.StrokeLine(buf, prevPx, prevPy, px, py, 1.0, edgeCol, false)
			}
			prevPx, prevPy = px, py
		}
	}

	// Composite buffer onto screen with team tint and low opacity.
	opts := &ebiten.DrawImageOptions{}
	opts.ColorScale.ScaleWithColor(tint)
	opts.ColorScale.ScaleAlpha(float32(opacity))
	screen.DrawImage(buf, opts)
}

// clipVisionRayToBuildings returns the endpoint of a vision ray after clipping
// against the closest building intersection.
func (g *Game) clipVisionRayToBuildings(ox, oy, angle, maxLen float64) (float64, float64) {
	ex := ox + math.Cos(angle)*maxLen
	ey := oy + math.Sin(angle)*maxLen

	bestT := 1.0
	hitAny := false
	for _, b := range g.buildings {
		t, hit := rayAABBHitT(
			ox, oy, ex, ey,
			float64(b.x), float64(b.y),
			float64(b.x+b.w), float64(b.y+b.h),
		)
		if hit && t < bestT {
			bestT = t
			hitAny = true
		}
	}

	if hitAny {
		clipT := math.Max(0, bestT-0.01)
		ex = ox + (ex-ox)*clipT
		ey = oy + (ey-oy)*clipT
	}

	return ex, ey
}

// drawGunfireLighting draws radial light blooms at active muzzle flash positions.
// Each bloom is a series of concentric translucent circles, fading with distance.
// Team-tinted: red/orange for friendlies, blue/cyan for OpFor.
func (g *Game) drawGunfireLighting(screen *ebiten.Image, offX, offY int) {
	ox, oy := float32(offX), float32(offY)
	flashes := g.combat.ActiveFlashes()
	for _, f := range flashes {
		fx := ox + float32(f.x)
		fy := oy + float32(f.y)
		progress := float32(f.age) / float32(flashLifetime)
		fade := 1.0 - progress

		var lR, lG, lB uint8
		if f.team == TeamRed {
			lR, lG, lB = 255, 160, 60 // warm orange bloom
		} else {
			lR, lG, lB = 80, 180, 255 // cool blue bloom
		}

		// Three concentric rings of decreasing opacity and increasing radius.
		vector.FillCircle(screen, fx, fy, 40*fade,
			color.RGBA{R: lR, G: lG, B: lB, A: uint8(30 * fade)}, false)
		vector.FillCircle(screen, fx, fy, 22*fade,
			color.RGBA{R: lR, G: lG, B: lB, A: uint8(50 * fade)}, false)
		vector.FillCircle(screen, fx, fy, 10*fade,
			color.RGBA{R: lR, G: lG, B: lB, A: uint8(80 * fade)}, false)
		// Hot white core.
		vector.FillCircle(screen, fx, fy, 4*fade,
			color.RGBA{R: 255, G: 255, B: 230, A: uint8(120 * fade)}, false)
	}
}

// drawVignette darkens the edges of the battlefield for atmosphere.
// Two-layer vignette: inner soft band + outer hard strip for depth.
func (g *Game) drawVignette(screen *ebiten.Image, offX, offY int) {
	ox, oy := float32(offX), float32(offY)
	gw, gh := float32(g.gameWidth), float32(g.gameHeight)

	// Outer hard strip — strong darkening at the absolute edge.
	outer := float32(40)
	outerDark := color.RGBA{R: 0, G: 0, B: 0, A: 80}
	vector.FillRect(screen, ox, oy, gw, outer, outerDark, false)
	vector.FillRect(screen, ox, oy+gh-outer, gw, outer, outerDark, false)
	vector.FillRect(screen, ox, oy, outer, gh, outerDark, false)
	vector.FillRect(screen, ox+gw-outer, oy, outer, gh, outerDark, false)

	// Inner soft band — subtle atmosphere gradient.
	inner := float32(120)
	innerDark := color.RGBA{R: 0, G: 0, B: 0, A: 30}
	vector.FillRect(screen, ox, oy, gw, inner, innerDark, false)
	vector.FillRect(screen, ox, oy+gh-inner, gw, inner, innerDark, false)
	vector.FillRect(screen, ox, oy, inner, gh, innerDark, false)
	vector.FillRect(screen, ox+gw-inner, oy, inner, gh, innerDark, false)
}

func drawGridOffset(screen *ebiten.Image, offX, offY, w, h, spacing int, c color.Color) {
	if spacing <= 0 {
		return
	}
	ox, oy := float32(offX), float32(offY)
	for x := 0; x <= w; x += spacing {
		xf := ox + float32(x)
		vector.StrokeLine(screen, xf, oy, xf, oy+float32(h), 1.0, c, false)
	}
	for y := 0; y <= h; y += spacing {
		yf := oy + float32(y)
		vector.StrokeLine(screen, ox, yf, ox+float32(w), yf, 1.0, c, false)
	}
}

func (g *Game) Layout(_, _ int) (int, int) {
	return g.width, g.height
}

// GameWidth returns the playfield width (excluding log panel).
func (g *Game) GameWidth() int {
	return g.gameWidth
}
