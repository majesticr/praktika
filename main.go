package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Zone struct {
	Main     ZoneArea `json:"main"`
	Buffer   ZoneArea `json:"buffer"`
	Corridor ZoneArea `json:"corridor"`
}

type ZoneArea struct {
	Area     []Point `json:"area"`
	Altitude float64 `json:"altitude"`
	Height   float64 `json:"height"`
}

type Drone struct {
	Name           string    `json:"name"`
	Position       Point     `json:"position"`
	Altitude       float64   `json:"altitude"`
	Color          string    `json:"color"`
	Speed          float64   `json:"speed"`
	Heading        float64   `json:"heading"`
	TargetPosition Point     `json:"-"`
	Corridor       []Point   `json:"corridor"`
	CorridorIndex  int       `json:"-"`
	IsReturning    bool      `json:"-"`
	ReturnPoint    Point     `json:"-"`
	LastInCorridor Point     `json:"-"`
	OutOfCorridor  bool      `json:"-"`
	DeviationStart time.Time `json:"-"`
}

type RouteZones map[string]Zone

type DroneState struct {
	CurrentZone string
	InMain      bool
	InBuffer    bool
	LastUpdate  time.Time
}

type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	DroneName string    `json:"drone_name"`
	Message   string    `json:"message"`
}

type DroneTracker struct {
	zones     RouteZones
	drones    map[string]*Drone
	states    map[string]*DroneState
	logs      []LogEntry
	mutex     sync.RWMutex
	upgrader  websocket.Upgrader
	clients   map[*websocket.Conn]bool
	broadcast chan []byte
}

// Инициализация параметров
func NewDroneTracker() *DroneTracker {
	return &DroneTracker{
		drones:    make(map[string]*Drone),
		states:    make(map[string]*DroneState),
		logs:      make([]LogEntry, 0),
		upgrader:  websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
		clients:   make(map[*websocket.Conn]bool),
		broadcast: make(chan []byte),
	}
}

// Проверка алгоритмами в т.ч. (ray casting) на пересечения зон и движения
func pointInsidePolygon(point Point, polygon []Point) bool {
	if len(polygon) < 3 {
		return false
	}
	x, y := point.X, point.Y
	inside := false
	j := len(polygon) - 1
	for i := 0; i < len(polygon); i++ {
		xi, yi := polygon[i].X, polygon[i].Y
		xj, yj := polygon[j].X, polygon[j].Y
		if ((yi > y) != (yj > y)) && (x < (xj-xi)*(y-yi)/(yj-yi)+xi) {
			inside = !inside
		}
		j = i
	}
	return inside
}

func (dt *DroneTracker) isInZone(drone *Drone, area ZoneArea) bool {
	if !pointInsidePolygon(drone.Position, area.Area) {
		return false
	}
	return drone.Altitude >= area.Altitude && drone.Altitude <= (area.Altitude+area.Height)
}

func (dt *DroneTracker) isInCorridor(drone *Drone) bool {
	if len(drone.Corridor) == 0 {
		return false
	}

	// Проверяем близость к любой точке коридора
	threshold := 0.004 // Увеличенный порог для коридора
	for _, point := range drone.Corridor {
		if distance(drone.Position, point) < threshold {
			return true
		}
	}
	return false
}

func distance(p1, p2 Point) float64 {
	dx := p1.X - p2.X
	dy := p1.Y - p2.Y
	return math.Sqrt(dx*dx + dy*dy)
}

// Состояние и движение
func (dt *DroneTracker) checkDronePosition(drone *Drone) string {
	dt.mutex.Lock()
	defer dt.mutex.Unlock()
	state, exists := dt.states[drone.Name]
	if !exists {
		state = &DroneState{}
		dt.states[drone.Name] = state
	}
	dt.checkSafetyCorridors(drone)
	zoneNames := []string{"Zone1", "Zone2", "Zone3", "Zone4", "Zone5", "Zone6", "Zone7", "Zone8", "Zone9", "Zone10"}
	var message string
	newInMain := false
	newInBuffer := false
	newCurrentZone := ""
	for _, zoneName := range zoneNames {
		zone, exists := dt.zones[zoneName]
		if !exists {
			continue
		}
		inMain := dt.isInZone(drone, zone.Main)
		inBuffer := dt.isInZone(drone, zone.Buffer)
		if inMain {
			newInMain = true
			newCurrentZone = zoneName
			if state.CurrentZone != zoneName || !state.InMain {
				if state.CurrentZone != "" && state.CurrentZone != zoneName {
					message = fmt.Sprintf("Дрон вылетел из %s области %s и влетел в main область %s",
						getAreaType(state.InMain), state.CurrentZone, zoneName)
				} else {
					message = fmt.Sprintf("Дрон внутри main области %s", zoneName)
				}
			}
			break
		} else if inBuffer {
			newInBuffer = true
			newCurrentZone = zoneName
			if state.CurrentZone != zoneName || !state.InBuffer {
				if state.CurrentZone != "" && state.CurrentZone != zoneName {
					message = fmt.Sprintf("Дрон вылетел из %s области %s и влетел в buffer область %s",
						getAreaType(state.InMain), state.CurrentZone, zoneName)
				} else {
					message = fmt.Sprintf("Дрон внутри buffer области %s", zoneName)
				}
			}
			break
		}
	}
	if !newInMain && !newInBuffer {
		if state.CurrentZone != "" {
			message = fmt.Sprintf("Дрон вылетел из %s области %s и покинул маршрут",
				getAreaType(state.InMain || state.InBuffer), state.CurrentZone)
		}
	}
	state.CurrentZone = newCurrentZone
	state.InMain = newInMain
	state.InBuffer = newInBuffer
	state.LastUpdate = time.Now()
	if message != "" {
		dt.addLog(drone.Name, message)
	}
	return message
}

func (dt *DroneTracker) checkSafetyCorridors(drone *Drone) {
	for _, otherDrone := range dt.drones {
		if otherDrone.Name == drone.Name {
			continue
		}
		distance := dt.calculateDistance(drone.Position, otherDrone.Position)
		if distance < 0.00009 { // ~10 метров
			dt.addLog(drone.Name, fmt.Sprintf("ПРЕДУПРЕЖДЕНИЕ: Дрон %s слишком близко к %s (%.1fм)",
				drone.Name, otherDrone.Name, distance*111000))
		}
	}
}

func (dt *DroneTracker) calculateDistance(p1, p2 Point) float64 {
	dx := p1.X - p2.X
	dy := p1.Y - p2.Y
	return math.Sqrt(dx*dx + dy*dy)
}

func getAreaType(inMain bool) string {
	if inMain {
		return "main"
	}
	return "buffer"
}

func (dt *DroneTracker) addLog(droneName, message string) {
	entry := LogEntry{
		Timestamp: time.Now(),
		DroneName: droneName,
		Message:   message,
	}
	dt.logs = append(dt.logs, entry)
	if len(dt.logs) > 1000 {
		dt.logs = dt.logs[len(dt.logs)-1000:]
	}
	logData, _ := json.Marshal(map[string]interface{}{
		"type": "log",
		"data": entry,
	})
	select {
	case dt.broadcast <- logData:
	default:
	}
	fmt.Printf("[%s] %s: %s\n", entry.Timestamp.Format("15:04:05"), droneName, message)
}

// Координаты коридоров
func (dt *DroneTracker) initializeCorridors() {
	corridors := map[string][]Point{
		"drone1": {
			{X: 50.080, Y: 53.190}, {X: 50.085, Y: 53.193}, {X: 50.090, Y: 53.195},
			{X: 50.095, Y: 53.198}, {X: 50.100, Y: 53.200}, {X: 50.105, Y: 53.202},
			{X: 50.110, Y: 53.205}, {X: 50.115, Y: 53.207}, {X: 50.120, Y: 53.210},
			{X: 50.125, Y: 53.212}, {X: 50.130, Y: 53.215}, {X: 50.135, Y: 53.218},
			{X: 50.140, Y: 53.220}, {X: 50.145, Y: 53.223}, {X: 50.150, Y: 53.225},
			{X: 50.155, Y: 53.227}, {X: 50.160, Y: 53.230}, {X: 50.165, Y: 53.232},
		},
		"drone2": {
			{X: 50.075, Y: 53.195}, {X: 50.080, Y: 53.197}, {X: 50.085, Y: 53.199},
			{X: 50.090, Y: 53.202}, {X: 50.095, Y: 53.205}, {X: 50.100, Y: 53.207},
			{X: 50.105, Y: 53.210}, {X: 50.110, Y: 53.213}, {X: 50.115, Y: 53.215},
			{X: 50.120, Y: 53.218}, {X: 50.125, Y: 53.220}, {X: 50.130, Y: 53.223},
			{X: 50.135, Y: 53.225}, {X: 50.140, Y: 53.228}, {X: 50.145, Y: 53.230},
			{X: 50.150, Y: 53.233}, {X: 50.155, Y: 53.235}, {X: 50.160, Y: 53.238},
		},
		"drone3": {
			{X: 50.070, Y: 53.200}, {X: 50.075, Y: 53.202}, {X: 50.080, Y: 53.205},
			{X: 50.085, Y: 53.208}, {X: 50.090, Y: 53.210}, {X: 50.095, Y: 53.213},
			{X: 50.100, Y: 53.215}, {X: 50.105, Y: 53.218}, {X: 50.110, Y: 53.220},
			{X: 50.115, Y: 53.223}, {X: 50.120, Y: 53.225}, {X: 50.125, Y: 53.228},
			{X: 50.130, Y: 53.230}, {X: 50.135, Y: 53.233}, {X: 50.140, Y: 53.235},
			{X: 50.145, Y: 53.238}, {X: 50.150, Y: 53.240}, {X: 50.155, Y: 53.243},
		},
		"drone4": {
			{X: 50.065, Y: 53.205}, {X: 50.070, Y: 53.208}, {X: 50.075, Y: 53.210},
			{X: 50.080, Y: 53.213}, {X: 50.085, Y: 53.215}, {X: 50.090, Y: 53.218},
			{X: 50.095, Y: 53.220}, {X: 50.100, Y: 53.223}, {X: 50.105, Y: 53.225},
			{X: 50.110, Y: 53.228}, {X: 50.115, Y: 53.230}, {X: 50.120, Y: 53.233},
			{X: 50.125, Y: 53.235}, {X: 50.130, Y: 53.238}, {X: 50.135, Y: 53.240},
			{X: 50.140, Y: 53.243}, {X: 50.145, Y: 53.245}, {X: 50.150, Y: 53.248},
		},
		"drone5": {
			{X: 50.060, Y: 53.210}, {X: 50.065, Y: 53.213}, {X: 50.070, Y: 53.215},
			{X: 50.075, Y: 53.218}, {X: 50.080, Y: 53.220}, {X: 50.085, Y: 53.223},
			{X: 50.090, Y: 53.225}, {X: 50.095, Y: 53.228}, {X: 50.100, Y: 53.230},
			{X: 50.105, Y: 53.233}, {X: 50.110, Y: 53.235}, {X: 50.115, Y: 53.238},
			{X: 50.120, Y: 53.240}, {X: 50.125, Y: 53.243}, {X: 50.130, Y: 53.245},
			{X: 50.135, Y: 53.248}, {X: 50.140, Y: 53.250}, {X: 50.145, Y: 53.253},
		},
		"drone6": {
			{X: 50.055, Y: 53.215}, {X: 50.060, Y: 53.218}, {X: 50.065, Y: 53.220},
			{X: 50.070, Y: 53.223}, {X: 50.075, Y: 53.225}, {X: 50.080, Y: 53.228},
			{X: 50.085, Y: 53.230}, {X: 50.090, Y: 53.233}, {X: 50.095, Y: 53.235},
			{X: 50.100, Y: 53.238}, {X: 50.105, Y: 53.240}, {X: 50.110, Y: 53.243},
			{X: 50.115, Y: 53.245}, {X: 50.120, Y: 53.248}, {X: 50.125, Y: 53.250},
			{X: 50.130, Y: 53.253}, {X: 50.135, Y: 53.255}, {X: 50.140, Y: 53.258},
		},
	}

	for droneName, corridor := range corridors {
		if drone, exists := dt.drones[droneName]; exists {
			drone.Corridor = corridor
			drone.CorridorIndex = 0
			drone.TargetPosition = corridor[0]
			drone.Position = corridor[0] // Старт с первой точки коридора
			drone.LastInCorridor = corridor[0]
			drone.IsReturning = false
			drone.OutOfCorridor = false

			// Добавляем лог о начале движения
			dt.addLog(droneName, fmt.Sprintf("Дрон инициализирован в позиции (%.3f, %.3f)", corridor[0].X, corridor[0].Y))
		}
	}
}

func (dt *DroneTracker) initializeZones() {
	dt.zones = RouteZones{
		"Zone1": Zone{
			Main: ZoneArea{
				Area: []Point{
					{X: 50.075, Y: 53.188},
					{X: 50.095, Y: 53.188},
					{X: 50.095, Y: 53.200},
					{X: 50.075, Y: 53.200},
				},
				Altitude: 250,
				Height:   100,
			},
			Buffer: ZoneArea{
				Area: []Point{
					{X: 50.070, Y: 53.185},
					{X: 50.100, Y: 53.185},
					{X: 50.100, Y: 53.203},
					{X: 50.070, Y: 53.203},
				},
				Altitude: 200,
				Height:   150,
			},
		},
		"Zone2": Zone{
			Main: ZoneArea{
				Area: []Point{
					{X: 50.090, Y: 53.195},
					{X: 50.110, Y: 53.195},
					{X: 50.110, Y: 53.210},
					{X: 50.090, Y: 53.210},
				},
				Altitude: 250,
				Height:   100,
			},
			Buffer: ZoneArea{
				Area: []Point{
					{X: 50.085, Y: 53.192},
					{X: 50.115, Y: 53.192},
					{X: 50.115, Y: 53.213},
					{X: 50.085, Y: 53.213},
				},
				Altitude: 200,
				Height:   150,
			},
		},
		"Zone3": Zone{
			Main: ZoneArea{
				Area: []Point{
					{X: 50.105, Y: 53.200},
					{X: 50.125, Y: 53.200},
					{X: 50.125, Y: 53.215},
					{X: 50.105, Y: 53.215},
				},
				Altitude: 250,
				Height:   100,
			},
			Buffer: ZoneArea{
				Area: []Point{
					{X: 50.100, Y: 53.197},
					{X: 50.130, Y: 53.197},
					{X: 50.130, Y: 53.218},
					{X: 50.100, Y: 53.218},
				},
				Altitude: 200,
				Height:   150,
			},
		},
		"Zone4": Zone{
			Main: ZoneArea{
				Area: []Point{
					{X: 50.120, Y: 53.205},
					{X: 50.140, Y: 53.205},
					{X: 50.140, Y: 53.220},
					{X: 50.120, Y: 53.220},
				},
				Altitude: 250,
				Height:   100,
			},
			Buffer: ZoneArea{
				Area: []Point{
					{X: 50.115, Y: 53.202},
					{X: 50.145, Y: 53.202},
					{X: 50.145, Y: 53.223},
					{X: 50.115, Y: 53.223},
				},
				Altitude: 200,
				Height:   150,
			},
		},
		"Zone5": Zone{
			Main: ZoneArea{
				Area: []Point{
					{X: 50.135, Y: 53.210},
					{X: 50.155, Y: 53.210},
					{X: 50.155, Y: 53.225},
					{X: 50.135, Y: 53.225},
				},
				Altitude: 250,
				Height:   100,
			},
			Buffer: ZoneArea{
				Area: []Point{
					{X: 50.130, Y: 53.207},
					{X: 50.160, Y: 53.207},
					{X: 50.160, Y: 53.228},
					{X: 50.130, Y: 53.228},
				},
				Altitude: 200,
				Height:   150,
			},
		},
		"Zone6": Zone{
			Main: ZoneArea{
				Area: []Point{
					{X: 50.150, Y: 53.215},
					{X: 50.170, Y: 53.215},
					{X: 50.170, Y: 53.235},
					{X: 50.150, Y: 53.235},
				},
				Altitude: 250,
				Height:   100,
			},
			Buffer: ZoneArea{
				Area: []Point{
					{X: 50.145, Y: 53.212},
					{X: 50.175, Y: 53.212},
					{X: 50.175, Y: 53.238},
					{X: 50.145, Y: 53.238},
				},
				Altitude: 200,
				Height:   150,
			},
		},
		"Zone7": Zone{
			Main: ZoneArea{
				Area: []Point{
					{X: 50.075, Y: 53.218},
					{X: 50.095, Y: 53.218},
					{X: 50.095, Y: 53.230},
					{X: 50.075, Y: 53.230},
				},
				Altitude: 250,
				Height:   100,
			},
			Buffer: ZoneArea{
				Area: []Point{
					{X: 50.070, Y: 53.215},
					{X: 50.100, Y: 53.215},
					{X: 50.100, Y: 53.233},
					{X: 50.070, Y: 53.233},
				},
				Altitude: 200,
				Height:   150,
			},
		},
		"Zone8": Zone{
			Main: ZoneArea{
				Area: []Point{
					{X: 50.090, Y: 53.225},
					{X: 50.110, Y: 53.225},
					{X: 50.110, Y: 53.240},
					{X: 50.090, Y: 53.240},
				},
				Altitude: 250,
				Height:   100,
			},
			Buffer: ZoneArea{
				Area: []Point{
					{X: 50.085, Y: 53.222},
					{X: 50.115, Y: 53.222},
					{X: 50.115, Y: 53.243},
					{X: 50.085, Y: 53.243},
				},
				Altitude: 200,
				Height:   150,
			},
		},
		"Zone9": Zone{
			Main: ZoneArea{
				Area: []Point{
					{X: 50.105, Y: 53.235},
					{X: 50.125, Y: 53.235},
					{X: 50.125, Y: 53.250},
					{X: 50.105, Y: 53.250},
				},
				Altitude: 250,
				Height:   100,
			},
			Buffer: ZoneArea{
				Area: []Point{
					{X: 50.100, Y: 53.232},
					{X: 50.130, Y: 53.232},
					{X: 50.130, Y: 53.253},
					{X: 50.100, Y: 53.253},
				},
				Altitude: 200,
				Height:   150,
			},
		},
		"Zone10": Zone{
			Main: ZoneArea{
				Area: []Point{
					{X: 50.120, Y: 53.245},
					{X: 50.140, Y: 53.245},
					{X: 50.140, Y: 53.260},
					{X: 50.120, Y: 53.260},
				},
				Altitude: 250,
				Height:   100,
			},
			Buffer: ZoneArea{
				Area: []Point{
					{X: 50.115, Y: 53.242},
					{X: 50.145, Y: 53.242},
					{X: 50.145, Y: 53.263},
					{X: 50.115, Y: 53.263},
				},
				Altitude: 200,
				Height:   150,
			},
		},
	}
}

// Цвета дронов, старт позиция, инициализация
func (dt *DroneTracker) initializeDrones() {
	colors := []string{"#FF0000", "#00FF00", "#0000FF", "#FFFF00", "#FF00FF", "#00FFFF"}
	startPositions := []Point{
		{X: 50.080, Y: 53.190},
		{X: 50.075, Y: 53.195},
		{X: 50.070, Y: 53.200},
		{X: 50.065, Y: 53.205},
		{X: 50.060, Y: 53.210},
		{X: 50.055, Y: 53.215},
	}

	for i := 1; i <= 6; i++ {
		drone := &Drone{
			Name:          fmt.Sprintf("drone%d", i),
			Position:      startPositions[i-1],
			Altitude:      275 + float64(i*5),
			Color:         colors[i-1],
			Speed:         15 + rand.Float64()*10,
			Heading:       45,
			CorridorIndex: 0,
			IsReturning:   false,
			OutOfCorridor: false,
		}
		dt.drones[drone.Name] = drone
	}
}

func (dt *DroneTracker) moveDroneAlongCorridor(drone *Drone) {
	if len(drone.Corridor) == 0 {
		return
	}

	// Проверка на отклонение от коридора с 2% шансом
	// Отключаем отклонения если дрон летит к началу маршрута (большое расстояние)
	currentTarget := drone.Corridor[drone.CorridorIndex]
	distanceToTarget := distance(drone.Position, currentTarget)
	isLongDistance := distanceToTarget > 0.01 // Если расстояние больше 0.01, то это полет к началу

	if !drone.OutOfCorridor && !drone.IsReturning && !isLongDistance && rand.Float64() < 0.02 {
		drone.OutOfCorridor = true
		drone.DeviationStart = time.Now()
		dt.addLog(drone.Name, "Дрон начал отклонение от коридора")
	}

	// Возвращение в коридор через 2-3 секунды
	if drone.OutOfCorridor && time.Since(drone.DeviationStart) > time.Duration(1+rand.Intn(2))*time.Second {
		drone.OutOfCorridor = false
		drone.IsReturning = true
		// Нахождение ближайшей точки коридора для возврата
		minDist := math.MaxFloat64
		closestIndex := 0
		for i, point := range drone.Corridor {
			dist := distance(drone.Position, point)
			if dist < minDist {
				minDist = dist
				closestIndex = i
			}
		}
		drone.CorridorIndex = closestIndex
		dt.addLog(drone.Name, "Дрон возвращается в коридор")
	}

	speed := 0.0002 // Базовая скорость

	if drone.OutOfCorridor {
		// Случайное движение при отклонении
		offsetX := (rand.Float64() - 0.5) * 0.003
		offsetY := (rand.Float64() - 0.5) * 0.003
		targetPoint := Point{
			X: drone.Position.X + offsetX,
			Y: drone.Position.Y + offsetY,
		}
		speed *= 0.2 // Медленнее при отклонении

		// Движение к целевой точке
		dx := targetPoint.X - drone.Position.X
		dy := targetPoint.Y - drone.Position.Y
		dist := math.Sqrt(dx*dx + dy*dy)

		if dist > 0 {
			dx /= dist
			dy /= dist
			drone.Position.X += dx * speed
			drone.Position.Y += dy * speed
			drone.Heading = math.Atan2(dy, dx) * 180 / math.Pi
			if drone.Heading < 0 {
				drone.Heading += 360
			}
		}

	} else if drone.IsReturning {
		// Возврат к коридору
		targetPoint := drone.Corridor[drone.CorridorIndex]
		speed *= 1.5 // Быстрее при возврате

		// Движение к целевой точке
		dx := targetPoint.X - drone.Position.X
		dy := targetPoint.Y - drone.Position.Y
		dist := math.Sqrt(dx*dx + dy*dy)

		if dist > 0 {
			dx /= dist
			dy /= dist
			drone.Position.X += dx * speed
			drone.Position.Y += dy * speed
			drone.Heading = math.Atan2(dy, dx) * 180 / math.Pi
			if drone.Heading < 0 {
				drone.Heading += 360
			}
		}

		// Проверка: достиг ли дрон коридора
		if dist < 0.0005 {
			drone.IsReturning = false
			dt.addLog(drone.Name, "Дрон вернулся в коридор")
		}

	} else {
		// Нормальное движение по коридору
		// Проверка: индекс в правильных границах
		if drone.CorridorIndex >= len(drone.Corridor) {
			drone.CorridorIndex = 0
		}

		targetPoint := drone.Corridor[drone.CorridorIndex]

		// Движение к целевой точке
		dx := targetPoint.X - drone.Position.X
		dy := targetPoint.Y - drone.Position.Y
		dist := math.Sqrt(dx*dx + dy*dy)

		if dist > 0 {
			dx /= dist
			dy /= dist
			drone.Position.X += dx * speed
			drone.Position.Y += dy * speed
			drone.Heading = math.Atan2(dy, dx) * 180 / math.Pi
			if drone.Heading < 0 {
				drone.Heading += 360
			}
		}

		// Проверка: достиг ли дрон текущей точки маршрута
		if dist < 0.0008 {
			// Переходим к следующей точке
			drone.CorridorIndex++

			// Если достигли конца маршрута, возвращаемся к началу
			if drone.CorridorIndex >= len(drone.Corridor) {
				drone.CorridorIndex = 0
				dt.addLog(drone.Name, "Дрон завершил маршрут и начинает новый круг")
			}
		}
	}

	// Плавное изменение по высоте
	if rand.Float64() < 0.03 {
		dz := (rand.Float64() - 0.5) * 3
		drone.Altitude = math.Max(220, math.Min(400, drone.Altitude+dz))
	}

	// Обновляем скорость для отображения
	drone.Speed = speed * 111000 * 3.6 // Конвертируем в км/ч

	// Ограничение области движения
	drone.Position.X = math.Max(50.040, math.Min(50.190, drone.Position.X))
	drone.Position.Y = math.Max(53.170, math.Min(53.270, drone.Position.Y))
}

// Движение дронов
func (dt *DroneTracker) simulateDroneMovement() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			dt.mutex.Lock()
			for _, drone := range dt.drones {
				dt.moveDroneAlongCorridor(drone)
			}
			dt.mutex.Unlock()
			for _, drone := range dt.drones {
				dt.checkDronePosition(drone)
			}
			dt.broadcastUpdate()
		}
	}
}

// HTTP и WEB Socket
func (dt *DroneTracker) broadcastUpdate() {
	dt.mutex.RLock()
	data := map[string]interface{}{
		"type":   "update",
		"drones": dt.drones,
		"zones":  dt.zones,
	}
	dt.mutex.RUnlock()
	message, err := json.Marshal(data)
	if err != nil {
		return
	}
	select {
	case dt.broadcast <- message:
	default:
	}
}

func (dt *DroneTracker) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := dt.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.Close()
	dt.clients[conn] = true
	dt.broadcastUpdate()
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			delete(dt.clients, conn)
			break
		}
	}
}

func (dt *DroneTracker) handleMessages() {
	for {
		select {
		case message := <-dt.broadcast:
			for client := range dt.clients {
				err := client.WriteMessage(websocket.TextMessage, message)
				if err != nil {
					client.Close()
					delete(dt.clients, client)
				}
			}
		}
	}
}

func (dt *DroneTracker) handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "static/index.html")
}

func (dt *DroneTracker) handleAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	dt.mutex.RLock()
	data := map[string]interface{}{
		"drones": dt.drones,
		"zones":  dt.zones,
		"logs":   dt.logs,
	}
	dt.mutex.RUnlock()
	json.NewEncoder(w).Encode(data)
}

func main() {
	tracker := NewDroneTracker()
	tracker.initializeZones()
	tracker.initializeDrones()
	tracker.initializeCorridors()
	go tracker.simulateDroneMovement()
	go tracker.handleMessages()
	http.HandleFunc("/", tracker.handleIndex)
	http.HandleFunc("/ws", tracker.handleWebSocket)
	http.HandleFunc("/api/data", tracker.handleAPI)
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	fmt.Println("Сервер запущен на http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
