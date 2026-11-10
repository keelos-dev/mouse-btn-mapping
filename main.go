package main

import (
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/jchv/go-webview2"
)

//go:embed ui.html
var uiHTML string

var (
	gmu   sync.Mutex
	mouse *Mouse
)

type ProfileInfo struct {
	Sector  int      `json:"sector"`
	Buttons []string `json:"buttons"`
	CRCOK   bool     `json:"crcOk"`
}

type DeviceInfo struct {
	Name        string        `json:"name"`
	PID         string        `json:"pid"`
	Mode        int           `json:"mode"`
	Current     int           `json:"current"`
	ButtonCount int           `json:"buttonCount"`
	Profiles    []ProfileInfo `json:"profiles"`
	BackupDir   string        `json:"backupDir"`
	Note        string        `json:"note"`
}

func dataDir() string {
	d := filepath.Join(os.Getenv("APPDATA"), "g502map")
	os.MkdirAll(d, 0o755)
	return d
}

func getMouse() (*Mouse, error) {
	if mouse != nil {
		if _, err := mouse.Mode(); err == nil {
			return mouse, nil
		}
		mouse.Close()
		mouse = nil
	}
	m, err := findMouse()
	if err != nil {
		return nil, err
	}
	mouse = m
	return m, nil
}

// backup dumps sector 0 and all given sectors: repeated [u16 sector][data].
func backup(m *Mouse, secs []uint16, path string) error {
	var out []byte
	for _, s := range append([]uint16{0}, secs...) {
		d, err := m.ReadSector(s)
		if err != nil {
			return err
		}
		out = binary.BigEndian.AppendUint16(out, s)
		out = append(out, d...)
	}
	return os.WriteFile(path, out, 0o644)
}

func origBackupPath(m *Mouse) string {
	return filepath.Join(dataDir(), fmt.Sprintf("original-%04x.bin", m.PID))
}

func connect() (*DeviceInfo, error) {
	gmu.Lock()
	defer gmu.Unlock()
	m, err := getMouse()
	if err != nil {
		return nil, err
	}
	info := &DeviceInfo{Name: m.Name, PID: fmt.Sprintf("%04X", m.PID), ButtonCount: m.ButtonCount, BackupDir: dataDir()}
	mode, err := m.Mode()
	if err != nil {
		return nil, err
	}
	info.Mode = int(mode)
	cur, _ := m.CurrentProfile()
	info.Current = int(cur)
	secs, err := m.ProfileSectors()
	if err != nil {
		info.Note = err.Error()
		return info, nil
	}
	if _, err := os.Stat(origBackupPath(m)); errors.Is(err, os.ErrNotExist) {
		if err := backup(m, secs, origBackupPath(m)); err != nil {
			return nil, fmt.Errorf("备份原始配置失败: %w", err)
		}
		info.Note = "已自动备份原始配置到 " + origBackupPath(m)
	}
	for _, s := range secs {
		d, err := m.ReadSector(s)
		if err != nil {
			return nil, err
		}
		p := ProfileInfo{Sector: int(s), CRCOK: sectorCRCOK(d)}
		for i := 0; i < m.ButtonCount; i++ {
			p.Buttons = append(p.Buttons, hex.EncodeToString(d[0x20+4*i:0x24+4*i]))
		}
		info.Profiles = append(info.Profiles, p)
	}
	return info, nil
}

func reload(m *Mouse) error {
	if mode, err := m.Mode(); err == nil && mode != 1 {
		if err := m.SetOnboardMode(); err != nil {
			return err
		}
	}
	cur, err := m.CurrentProfile()
	if err != nil {
		return err
	}
	return m.SetCurrentProfile(cur)
}

func writeButtons(sector int, buttons []string) error {
	gmu.Lock()
	defer gmu.Unlock()
	m, err := getMouse()
	if err != nil {
		return err
	}
	if len(buttons) != m.ButtonCount {
		return errors.New("按键数量不对")
	}
	secs, err := m.ProfileSectors()
	if err != nil {
		return err
	}
	ok := false
	for _, s := range secs {
		ok = ok || int(s) == sector
	}
	if !ok {
		return fmt.Errorf("扇区 %d 不是配置扇区", sector)
	}
	d, err := m.ReadSector(uint16(sector))
	if err != nil {
		return err
	}
	for i, h := range buttons {
		b, err := hex.DecodeString(h)
		if err != nil || len(b) != 4 {
			return fmt.Errorf("按键 %d 编码错误: %q", i, h)
		}
		copy(d[0x20+4*i:], b)
	}
	bk := filepath.Join(dataDir(), "backup-"+time.Now().Format("20060102-150405")+".bin")
	if err := backup(m, secs, bk); err != nil {
		return fmt.Errorf("写入前备份失败: %w", err)
	}
	if err := m.WriteSector(uint16(sector), d); err != nil {
		return err
	}
	return reload(m)
}

// factoryReset copies the ROM profiles over the user profiles (and writes the
// directory if it is empty).
func factoryReset() error {
	gmu.Lock()
	defer gmu.Unlock()
	m, err := getMouse()
	if err != nil {
		return err
	}
	romDir, err := m.ReadSector(0x100)
	if err != nil {
		return err
	}
	var roms []uint16
	for i := 0; i+4 <= 64 && len(roms) < m.ProfileCount; i += 4 {
		s := uint16(romDir[i])<<8 | uint16(romDir[i+1])
		if s == 0xffff || s < 0x100 {
			break
		}
		roms = append(roms, s)
	}
	if len(roms) == 0 {
		return errors.New("读不到 ROM 默认配置")
	}
	secs, err := m.ProfileSectors()
	if err != nil {
		// empty directory: create sectors 1..n
		secs = nil
		dir := make([]byte, m.SectorSize)
		for i := range dir {
			dir[i] = 0xff
		}
		for i := range roms {
			s := uint16(i + 1)
			secs = append(secs, s)
			dir[4*i], dir[4*i+1], dir[4*i+2], dir[4*i+3] = byte(s>>8), byte(s), 1, 0
		}
		if err := m.WriteSector(0, dir); err != nil {
			return fmt.Errorf("写目录: %w", err)
		}
	} else {
		bk := filepath.Join(dataDir(), "backup-"+time.Now().Format("20060102-150405")+".bin")
		if err := backup(m, secs, bk); err != nil {
			return err
		}
	}
	for i, s := range secs {
		if i >= len(roms) {
			break
		}
		d, err := m.ReadSector(roms[i])
		if err != nil {
			return err
		}
		if err := m.WriteSector(s, d); err != nil {
			return err
		}
	}
	return reload(m)
}

func restoreOriginal() error {
	gmu.Lock()
	defer gmu.Unlock()
	m, err := getMouse()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(origBackupPath(m))
	if err != nil {
		return fmt.Errorf("没有原始备份: %w", err)
	}
	step := 2 + m.SectorSize
	if len(raw)%step != 0 {
		return errors.New("备份文件格式不对")
	}
	for off := 0; off < len(raw); off += step {
		s := binary.BigEndian.Uint16(raw[off:])
		if s == 0 {
			continue // directory unchanged by this tool
		}
		if err := m.WriteSector(s, raw[off+2:off+step]); err != nil {
			return err
		}
	}
	return reload(m)
}

func openBackupDir() { exec.Command("explorer", dataDir()).Start() }

func main() {
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  filepath.Join(dataDir(), "webview"),
		WindowOptions: webview2.WindowOptions{
			Title: "G502 板载按键映射", Width: 980, Height: 760, Center: true,
		},
	})
	if w == nil {
		log.Fatal("WebView2 初始化失败（需要 Microsoft Edge WebView2 Runtime）")
	}
	defer w.Destroy()
	w.Bind("goConnect", connect)
	w.Bind("goWrite", writeButtons)
	w.Bind("goFactory", factoryReset)
	w.Bind("goRestore", restoreOriginal)
	w.Bind("goOpenDir", openBackupDir)
	w.SetHtml(uiHTML)
	w.Run()
}
