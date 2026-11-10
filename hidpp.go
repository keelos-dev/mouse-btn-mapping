package main

// HID++ 2.0 over the long-report (0x11) vendor collection, feature 0x8100
// (Onboard Profiles). Layout verified on G502 Proteus Spectrum (C332):
// 256-byte sectors, button bindings at 0x20 (4 bytes each), CRC-CCITT at end.

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	logiVID  = 0x046d
	swID     = 0x0a
	featRoot = 0x0000
	featOnb  = 0x8100
)

type Mouse struct {
	mu           sync.Mutex
	dev          *hidDev
	PID          uint16
	Name         string
	onb          byte
	SectorSize   int
	ProfileCount int
	ButtonCount  int
	RomCount     int
}

func findMouse() (*Mouse, error) {
	list, err := hidEnumerate(logiVID)
	if err != nil {
		return nil, err
	}
	var lastErr error = errors.New("没找到罗技 HID++ 设备（请确认鼠标已插上，且 G HUB/LGS 没在运行）")
	for _, info := range list {
		if info.UsagePage != 0xff00 || info.Usage != 0x0002 || info.InLen < 20 {
			continue
		}
		d, err := hidOpen(info)
		if err != nil {
			lastErr = err
			continue
		}
		m := &Mouse{dev: d, PID: info.PID, Name: info.Product}
		if err := m.init(); err != nil {
			d.Close()
			lastErr = fmt.Errorf("%04x: %w", info.PID, err)
			continue
		}
		return m, nil
	}
	return nil, lastErr
}

func (m *Mouse) Close() { m.dev.Close() }

func (m *Mouse) req(feat, fn byte, params []byte) ([]byte, error) {
	pkt := make([]byte, 20)
	pkt[0], pkt[1], pkt[2], pkt[3] = 0x11, 0xff, feat, fn<<4|swID
	copy(pkt[4:], params)
	for attempt := 0; attempt < 3; attempt++ {
		if err := m.dev.Write(pkt); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(800 * time.Millisecond)
		for time.Now().Before(deadline) {
			r, err := m.dev.Read(300)
			if err == errTimeout {
				continue
			}
			if err != nil {
				return nil, err
			}
			if len(r) < 20 || r[1] != 0xff {
				continue
			}
			if r[2] == 0xff && r[3] == feat && r[4] == pkt[3] {
				return nil, fmt.Errorf("HID++ 错误 0x%02x (feat %d fn %d)", r[5], feat, fn)
			}
			if r[2] == feat && r[3] == pkt[3] {
				return r[4:20], nil
			}
		}
	}
	return nil, errors.New("设备无响应")
}

func (m *Mouse) init() error {
	r, err := m.req(0, 0, []byte{featOnb >> 8, featOnb & 0xff})
	if err != nil {
		return err
	}
	if r[0] == 0 {
		return errors.New("设备不支持板载配置 (0x8100)")
	}
	m.onb = r[0]
	d, err := m.req(m.onb, 0, nil)
	if err != nil {
		return err
	}
	// memModel, profFmt, macroFmt, profileCount, profileCountOOB, buttonCount, sectorCount, sectorSize(2)
	m.ProfileCount = int(d[3])
	m.RomCount = int(d[4])
	m.ButtonCount = int(d[5])
	m.SectorSize = int(d[7])<<8 | int(d[8])
	if m.SectorSize < 0x80 || m.SectorSize > 0x1000 || m.SectorSize%16 != 0 {
		return fmt.Errorf("意外的扇区大小 %d", m.SectorSize)
	}
	if m.ButtonCount > 16 {
		m.ButtonCount = 16
	}
	return nil
}

func (m *Mouse) Mode() (byte, error) {
	r, err := m.req(m.onb, 2, nil)
	if err != nil {
		return 0, err
	}
	return r[0], nil
}

func (m *Mouse) SetOnboardMode() error {
	_, err := m.req(m.onb, 1, []byte{1})
	return err
}

func (m *Mouse) CurrentProfile() (uint16, error) {
	r, err := m.req(m.onb, 4, nil)
	if err != nil {
		return 0, err
	}
	return uint16(r[0])<<8 | uint16(r[1]), nil
}

func (m *Mouse) SetCurrentProfile(sec uint16) error {
	_, err := m.req(m.onb, 3, []byte{byte(sec >> 8), byte(sec)})
	return err
}

func (m *Mouse) ReadSector(sec uint16) ([]byte, error) {
	out := make([]byte, m.SectorSize)
	for off := 0; off < m.SectorSize; off += 16 {
		r, err := m.req(m.onb, 5, []byte{byte(sec >> 8), byte(sec), byte(off >> 8), byte(off)})
		if err != nil {
			return nil, fmt.Errorf("读扇区 %04x+%x: %w", sec, off, err)
		}
		copy(out[off:], r[:16])
	}
	return out, nil
}

func crcCCITT(b []byte) uint16 {
	c := uint16(0xffff)
	for _, x := range b {
		c ^= uint16(x) << 8
		for i := 0; i < 8; i++ {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x1021
			} else {
				c <<= 1
			}
		}
	}
	return c
}

func sectorCRCOK(b []byte) bool {
	n := len(b)
	return crcCCITT(b[:n-2]) == uint16(b[n-2])<<8|uint16(b[n-1])
}

// WriteSector writes a whole user sector (CRC is recomputed) and reads it back.
func (m *Mouse) WriteSector(sec uint16, data []byte) error {
	if sec >= 0x100 { // ROM area

		return fmt.Errorf("拒绝写扇区 %04x", sec)
	}
	if len(data) != m.SectorSize {
		return errors.New("扇区长度不对")
	}
	buf := append([]byte(nil), data...)
	n := len(buf)
	c := crcCCITT(buf[:n-2])
	buf[n-2], buf[n-1] = byte(c>>8), byte(c)
	if _, err := m.req(m.onb, 6, []byte{byte(sec >> 8), byte(sec), 0, 0, byte(n >> 8), byte(n)}); err != nil {
		return fmt.Errorf("开始写: %w", err)
	}
	for off := 0; off < n; off += 16 {
		if _, err := m.req(m.onb, 7, buf[off:off+16]); err != nil {
			return fmt.Errorf("写 +%x: %w", off, err)
		}
	}
	if _, err := m.req(m.onb, 8, nil); err != nil {
		return fmt.Errorf("结束写: %w", err)
	}
	back, err := m.ReadSector(sec)
	if err != nil {
		return err
	}
	for i := range buf {
		if back[i] != buf[i] {
			return fmt.Errorf("回读校验失败 @%x", i)
		}
	}
	return nil
}

// ProfileSectors returns the user profile sectors listed in the directory (sector 0).
func (m *Mouse) ProfileSectors() ([]uint16, error) {
	dir, err := m.ReadSector(0)
	if err != nil {
		return nil, err
	}
	var secs []uint16
	for i := 0; i+4 <= len(dir)-2 && len(secs) < m.ProfileCount; i += 4 {
		s := uint16(dir[i])<<8 | uint16(dir[i+1])
		if s == 0xffff {
			break
		}
		secs = append(secs, s)
	}
	if len(secs) == 0 {
		return nil, errors.New("目录扇区为空（鼠标在用 ROM 默认配置），请先「恢复出厂」生成用户配置")
	}
	return secs, nil
}
