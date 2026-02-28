package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// SmartctlOutput maps the JSON output of smartctl
type SmartctlOutput struct {
	Device struct {
		Name     string `json:"name"`
		InfoName string `json:"info_name"`
		Type     string `json:"type"`
	} `json:"device"`
	ModelName    string `json:"model_name"`
	SerialNumber string `json:"serial_number"`
	UserCapacity struct {
		Bytes int64 `json:"bytes"`
	} `json:"user_capacity"`
	Temperature struct {
		Current int `json:"current"`
	} `json:"temperature"`
	PowerCycleCount int `json:"power_cycle_count"`
	PowerOnTime     struct {
		Hours int `json:"hours"`
	} `json:"power_on_time"`
	AtaSmartAttributes struct {
		Table []SmartAttribute `json:"table"`
	} `json:"ata_smart_attributes"`
	NvmeSmartHealthInformationLog struct {
		CriticalWarning         int `json:"critical_warning"`
		Temperature             int `json:"temperature"`
		AvailableSpare          int `json:"available_spare"`
		PercentageUsed          int `json:"percentage_used"`
		PowerCycles             int `json:"power_cycles"`
		PowerOnHours            int `json:"power_on_hours"`
	} `json:"nvme_smart_health_information_log"`
}

type SmartscanOutput struct {
	Devices []struct {
		Name     string `json:"name"`
		InfoName string `json:"info_name"`
		Type     string `json:"type"`
	} `json:"devices"`
}

type SmartAttribute struct {
	Id  int `json:"id"`
	Raw struct {
		Value int `json:"value"`
	} `json:"raw"`
}

// DiskHealth holds parsed data
type DiskHealth struct {
	Model           string
	Serial          string
	CapacityGB      float64
	Temperature     int
	HealthScore     int // 0 to 100
	IsNVMe          bool
	PowerOnHours    int
	PowerCycleCount int
}

// 1. Data Retrieval and JSON Parsing
func getDiskData(devicePath string) (*DiskHealth, error) {
	// SUDO / ROOT PERMISSIONS HANDLING 
	// ======================================
	// Reading raw S.M.A.R.T. data usually requires root permissions.
	// The user is expected to run this as: `sudo disk-analyzer analyze /dev/sda`
	// Or we could detect it via os.Getuid() == 0 on Unix systems.
	// For this MVP, we delegate running with sudo to the user, but warn if it fails.

	cmd := exec.Command("smartctl", "-j", "-a", devicePath)
	
	output, err := cmd.Output()
	
	// If there was an error but smartctl returned text, try to parse it (non-zero status can be a disk warning).
	if err != nil && len(output) == 0 {
		return nil, fmt.Errorf("error running smartctl: %w\n  Make sure to run the program with 'sudo' or Administrator privileges.", err)
	}

	var smartData SmartctlOutput
	if err := json.Unmarshal(output, &smartData); err != nil {
		return nil, fmt.Errorf("error parsing JSON from smartctl: %w", err)
	}

	info := &DiskHealth{
		Model:           smartData.ModelName,
		Serial:          smartData.SerialNumber,
		CapacityGB:      float64(smartData.UserCapacity.Bytes) / (1024 * 1024 * 1024),
		Temperature:     smartData.Temperature.Current,
		PowerOnHours:    smartData.PowerOnTime.Hours,
		PowerCycleCount: smartData.PowerCycleCount,
	}
	
	if info.Model == "" && smartData.Device.InfoName != "" {
		info.Model = smartData.Device.InfoName
	}
	
	if smartData.Device.Type == "nvme" || smartData.NvmeSmartHealthInformationLog.Temperature > 0 {
		info.IsNVMe = true
		if info.Temperature == 0 {
			info.Temperature = smartData.NvmeSmartHealthInformationLog.Temperature
		}
		if info.PowerOnHours == 0 {
			info.PowerOnHours = smartData.NvmeSmartHealthInformationLog.PowerOnHours
		}
		if info.PowerCycleCount == 0 {
			info.PowerCycleCount = smartData.NvmeSmartHealthInformationLog.PowerCycles
		}
	} else {
		// Fallback for SATA if older smartctl doesn't expose root power props
		if info.PowerOnHours == 0 || info.PowerCycleCount == 0 {
			for _, attr := range smartData.AtaSmartAttributes.Table {
				if attr.Id == 9 && info.PowerOnHours == 0 {
					info.PowerOnHours = attr.Raw.Value
				}
				if attr.Id == 12 && info.PowerCycleCount == 0 {
					info.PowerCycleCount = attr.Raw.Value
				}
			}
		}
	}

	info.HealthScore = calculateHealthPercentage(&smartData)

	return info, nil
}

type DiskOption struct {
	Path       string
	Model      string
	Serial     string
	CapacityGB float64
}

func getAvailableDisks() ([]DiskOption, error) {
	var devicePaths []string
	var disks []DiskOption

	// 1. Scan via smartctl (detects OS-agnostic disks)
	cmd := exec.Command("smartctl", "--scan", "-j")
	output, _ := cmd.Output()
	if len(output) > 0 {
		var scanData SmartscanOutput
		if err := json.Unmarshal(output, &scanData); err == nil {
			for _, dev := range scanData.Devices {
				// Ignore temporary IOService paths because on MacOS we prefer to find fixed /dev/disk* nodes
				if !strings.HasPrefix(dev.Name, "IOService:/") {
					devicePaths = append(devicePaths, dev.Name)
				}
			}
		}
	}

	// 2. Extract classic BSD Names or Linux Names from /dev/ and group everything
	globPaths := []string{"/dev/disk[0-9]", "/dev/disk[0-9][0-9]", "/dev/sd[a-z]", "/dev/nvme[0-9]n[0-9]"}
	for _, pattern := range globPaths {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			found := false
			for _, existing := range devicePaths {
				if existing == m {
					found = true
					break
				}
			}
			if !found {
				devicePaths = append(devicePaths, m)
			}
		}
	}

	// 3. Sort them properly (e.g. disk1, disk2, disk10) by numeric suffix if possible
	sort.Slice(devicePaths, func(i, j int) bool {
		re := regexp.MustCompile(`([0-9]+)$`)
		mI := re.FindStringSubmatch(devicePaths[i])
		mJ := re.FindStringSubmatch(devicePaths[j])
		if len(mI) > 1 && len(mJ) > 1 {
			numI, _ := strconv.Atoi(mI[1])
			numJ, _ := strconv.Atoi(mJ[1])
			if numI != numJ {
				return numI < numJ
			}
		}
		return devicePaths[i] < devicePaths[j]
	})

	// 4. Test with getDiskData
	for _, devPath := range devicePaths {
		data, err := getDiskData(devPath)
		if err == nil && data != nil {
			disks = append(disks, DiskOption{
				Path:       devPath,
				Model:      data.Model,
				Serial:     data.Serial,
				CapacityGB: data.CapacityGB,
			})
		}
	}

	// (MacOS Fallback): If for some reason /dev/diskX yielded "0" responses,
	// try grabbing "IOService" (less readable but definitely detectable)
	if len(disks) == 0 && len(output) > 0 {
		var scanData SmartscanOutput
		json.Unmarshal(output, &scanData)
		for _, dev := range scanData.Devices {
			if strings.HasPrefix(dev.Name, "IOService:/") {
				data, err := getDiskData(dev.Name)
				if err == nil && data != nil {
					disks = append(disks, DiskOption{
						Path:       dev.Name,
						Model:      data.Model,
						Serial:     data.Serial,
						CapacityGB: data.CapacityGB,
					})
				}
			}
		}
	}

	if len(disks) == 0 {
		return nil, fmt.Errorf("no supported disks detected")
	}

	return disks, nil
}

// 2. Health Logic (Heuristics)
// Returns a percentage between 0 and 100.
func calculateHealthPercentage(smartData *SmartctlOutput) int {
	health := 100

	if smartData.Device.Type == "nvme" || smartData.NvmeSmartHealthInformationLog.Temperature > 0 {
		// Basic heuristics for NVMe
		health -= smartData.NvmeSmartHealthInformationLog.PercentageUsed
		if smartData.NvmeSmartHealthInformationLog.CriticalWarning > 0 {
			health -= 50
		}
		if health < 0 { health = 0 }
		return health
	}

	// Heuristics for SATA (based on critical parameters)
	// ID 5: Reallocated_Sector_Ct
	// ID 197: Current_Pending_Sector
	// ID 198: Offline_Uncorrectable
	for _, attr := range smartData.AtaSmartAttributes.Table {
		switch attr.Id {
		case 5, 197, 198:
			// For each raw value > 0 in these critical attributes, subtract 10% health.
			rawVal := attr.Raw.Value
			if rawVal > 0 {
				health -= (rawVal * 10)
			}
		}
	}

	if health < 0 {
		health = 0
	}

	return health
}

// 3. UI Frontend with BubbleTea/Lipgloss
var (
	titleStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#FAFAFA")).
		Background(lipgloss.Color("#7D56F4")).
		Padding(0, 1).
		MarginBottom(1)

	labelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#A0A0A0")).Width(18)
	valueStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#E0E0E0")).Bold(true)

	// Health color rules
	colorGreen  = lipgloss.Color("#04B575") // 80% to 100%
	colorYellow = lipgloss.Color("#F8D800") // 40% to 79%
	colorRed    = lipgloss.Color("#FF3366") // Under 40%
	colorBg     = lipgloss.Color("#333333") // Progress bar background
)

type sessionState int

const (
	stateSelection sessionState = iota
	stateAnalysis
)

type model struct {
	state    sessionState
	disks    []DiskOption
	cursor   int
	diskData *DiskHealth
	err      error
}

type diskDataMsg struct {
	data *DiskHealth
	err  error
}

func fetchDiskDataCmd(devicePath string) tea.Cmd {
	return func() tea.Msg {
		data, err := getDiskData(devicePath)
		return diskDataMsg{data: data, err: err}
	}
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Handle key presses
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "up", "k":
			if m.state == stateSelection && m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.state == stateSelection && m.cursor < len(m.disks)-1 {
				m.cursor++
			}
		case "enter":
			if m.state == stateSelection {
				if len(m.disks) == 0 {
					return m, nil
				}
				m.state = stateAnalysis
				m.diskData = nil
				m.err = nil
				return m, fetchDiskDataCmd(m.disks[m.cursor].Path)
			}
		case "esc", "backspace":
			if m.state == stateAnalysis {
				m.state = stateSelection
			}
		}
	
	case diskDataMsg:
		m.diskData = msg.data
		m.err = msg.err
	}

	return m, nil
}

func (m model) View() string {
	if m.state == stateSelection {
		s := "\n" + titleStyle.Render("💽 Select a disk to analyze") + "\n\n"
		if m.err != nil {
			s += fmt.Sprintf("  ❌ Error: %v\n\n  Press 'q' to quit.\n", m.err)
		} else if len(m.disks) == 0 {
			s += "  No disks detected.\n  Make sure to run with 'sudo'.\n\n"
		} else {
			for i, disk := range m.disks {
				cursorStr := "  "
				if m.cursor == i {
					cursorStr = lipgloss.NewStyle().Foreground(lipgloss.Color("#7D56F4")).Bold(true).Render("► ")
				}
				
				modelName := disk.Model
				
				// Clean up enormous Apple Mac IOService PATHS
				if strings.HasPrefix(modelName, "IOService:/") {
					parts := strings.Split(modelName, "/")
					if len(parts) > 0 {
						modelName = parts[len(parts)-1]
					}
				}
				if modelName == "" {
					modelName = "Unknown Device"
				}

				label := modelName
				if disk.Serial != "" {
					label += fmt.Sprintf(" (S/N: %s)", disk.Serial)
				}
				if disk.CapacityGB > 0 {
					label += fmt.Sprintf(" %.1f GB", disk.CapacityGB)
				}
				if disk.Path != "" && !strings.HasPrefix(disk.Path, "IOService") {
					label += fmt.Sprintf(" (%s)", filepath.Base(disk.Path))
				}

				s += fmt.Sprintf(" %s%s\n", cursorStr, label)
			}
		}
		s += "\n  " + lipgloss.NewStyle().Foreground(lipgloss.Color("#777777")).Render("↑/↓: Navigate • Enter: Analyze • q: Quit") + "\n\n"
		return s
	}

	if m.err != nil {
		return fmt.Sprintf("\n  ❌ Error: %v\n\n  Press 'esc' to go back or 'q' to quit.\n", m.err)
	}

	if m.diskData == nil {
		return "\n  Loading disk data...\n"
	}

	s := "\n" + titleStyle.Render("📊 Disk Health Dashboard") + "\n\n"

	s += fmt.Sprintf("  %s %s\n", labelStyle.Render("Model:"), valueStyle.Render(m.diskData.Model))
	s += fmt.Sprintf("  %s %s\n", labelStyle.Render("Serial:"), valueStyle.Render(m.diskData.Serial))
	
	capStr := fmt.Sprintf("%.2f GB", m.diskData.CapacityGB)
	s += fmt.Sprintf("  %s %s\n", labelStyle.Render("Capacity:"), valueStyle.Render(capStr))
	
	tempStr := fmt.Sprintf("%d °C", m.diskData.Temperature)
	s += fmt.Sprintf("  %s %s\n", labelStyle.Render("Temperature:"), valueStyle.Render(tempStr))

	hoursStr := fmt.Sprintf("%d hrs", m.diskData.PowerOnHours)
	s += fmt.Sprintf("  %s %s\n", labelStyle.Render("Power On Hours:"), valueStyle.Render(hoursStr))

	cyclesStr := fmt.Sprintf("%d", m.diskData.PowerCycleCount)
	s += fmt.Sprintf("  %s %s\n", labelStyle.Render("Power Cycles:"), valueStyle.Render(cyclesStr))

	tipoStr := "SATA / HDD / SSD"
	if m.diskData.IsNVMe { tipoStr = "NVMe" }
	s += fmt.Sprintf("  %s %s\n", labelStyle.Render("Type:"), valueStyle.Render(tipoStr))
	s += "\n"

	s += "  " + labelStyle.Render("Global Health:") + "\n"
	s += "  " + renderProgressBar(m.diskData.HealthScore) + "\n\n"
	s += "  " + lipgloss.NewStyle().Foreground(lipgloss.Color("#777777")).Render("Press 'esc' to safely go back or 'q' to quit.") + "\n\n"
	
	return s
}

// renderProgressBar creates a visual progress bar using fixed width blocks
func renderProgressBar(percent int) string {
	width := 50
	filled := int(float64(width) * (float64(percent) / 100.0))
	empty := width - filled

	var barColor lipgloss.Color
	if percent >= 80 {
		barColor = colorGreen
	} else if percent >= 40 {
		barColor = colorYellow
	} else {
		barColor = colorRed
	}

	filledStyle := lipgloss.NewStyle().Background(barColor)
	emptyStyle := lipgloss.NewStyle().Background(colorBg)

	// Use background colored spaces to create solid blocks
	bar := filledStyle.Render(strings.Repeat(" ", filled)) + emptyStyle.Render(strings.Repeat(" ", empty))
	percentText := lipgloss.NewStyle().Bold(true).Foreground(barColor).Render(fmt.Sprintf(" %d%%", percent))
	
	return bar + percentText
}

// 4. Pre-flight Checks
var (
	dependencyBoxStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#FF8C00")). // Warning Orange
		Padding(1, 3).
		Margin(1, 2)

	dependencyTitleStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#FF3366")).
		Bold(true)

	codeHighlightStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#04B575")).
		Bold(true)

	privilegeBoxStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#04B575")). 
		Padding(1, 3).
		Margin(1, 2)

	privilegeTitleStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#04B575")).
		Bold(true)
)

func checkDependencies() bool {
	_, err := exec.LookPath("smartctl")
	if err == nil {
		return true
	}

	var installInstruction string
	switch runtime.GOOS {
	case "darwin":
		installInstruction = "brew install smartmontools"
	case "windows":
		installInstruction = "choco install smartmontools\nOr download from official website."
	default: // mostly linux
		installInstruction = "sudo apt install smartmontools"
	}

	title := dependencyTitleStyle.Render("⚠️  Missing Dependency: smartmontools")
	
	body := "This tool requires " + codeHighlightStyle.Render("smartctl") + " to physically\nread the health data from your drives.\n\n"
	body += "How to install it on your OS (" + runtime.GOOS + "):\n"
	body += "  > " + codeHighlightStyle.Render(installInstruction)

	dialog := lipgloss.JoinVertical(lipgloss.Left, title, body)
	fmt.Println(dependencyBoxStyle.Render(dialog))

	return false
}

func checkRootPrivileges() bool {
	if runtime.GOOS == "windows" {
		// Basic Windows check could involve opening a system file or calling syscalls, 
		// but since smartctl often prompts for elevation or fails natively, we'll use a basic check or just 
		// show a warning if basic disk scanning fails later.
		return true 
	}

	// Linux / macOS logic
	if os.Geteuid() == 0 {
		return true
	}

	title := privilegeTitleStyle.Render("🛡️  Insufficient Privileges")
	body := "To read physical status and health data from your drives,\nthe tool needs direct low-level hardware access.\n\n"
	body += "Please, run the program again using:\n"
	body += "  > " + codeHighlightStyle.Render("sudo " + filepath.Base(os.Args[0]))
	
	dialog := lipgloss.JoinVertical(lipgloss.Left, title, body)
	fmt.Println(privilegeBoxStyle.Render(dialog))

	return false
}

func main() {
	if !checkDependencies() {
		os.Exit(1)
	}

	if !checkRootPrivileges() {
		os.Exit(1)
	}

	var rootCmd = &cobra.Command{
		Use:   "disk-analyzer",
		Short: "Disk Analyzer TUI MVP",
		Long:  "Terminal tool written in Go that shows a minimalist Dashboard (BubbleTea) checking disk health from smartctl.",
		Run: func(cmd *cobra.Command, args []string) {
			disks, err := getAvailableDisks()
			m := model{
				state: stateSelection,
				disks: disks,
				err:   err,
			}
			p := tea.NewProgram(m)
			if _, err := p.Run(); err != nil {
				fmt.Printf("Error starting TUI: %v\n", err)
				os.Exit(1)
			}
		},
	}

	var analyzeCmd = &cobra.Command{
		Use:   "analyze [disk_path]",
		Short: "Analyzes the state of a specific disk (e.g. /dev/sda)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			devicePath := args[0]
			diskData, err := getDiskData(devicePath)
			
			// Initialize and launch directly into "Analysis" mode
			m := model{
				state:    stateAnalysis,
				diskData: diskData,
				err:      err,
			}
			p := tea.NewProgram(m)
			if _, err := p.Run(); err != nil {
				fmt.Printf("Error starting TUI: %v\n", err)
				os.Exit(1)
			}
		},
	}

	rootCmd.AddCommand(analyzeCmd)
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
