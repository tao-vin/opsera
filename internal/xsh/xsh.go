package xsh

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

type Session struct {
	Path     string
	Host     string
	Port     int
	UserName string
	Password string
}

func Parse(path string) (Session, error) {
	file, err := os.Open(path)
	if err != nil {
		return Session{}, err
	}
	defer file.Close()
	out := Session{Path: path, Port: 22}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "[") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Host":
			out.Host = strings.TrimSpace(value)
		case "Port":
			if port, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				out.Port = port
			}
		case "UserName":
			out.UserName = strings.TrimSpace(value)
		case "Password":
			out.Password = strings.TrimSpace(value)
		}
	}
	return out, scanner.Err()
}
