package port_pool

import (
	"encoding/json"
	"fmt"
	"os"
)

type States []State

type State struct {
	Offset uint32 `json:"offset"`
}

func LoadState(filePath string) (States, error) {
	stateFile, err := os.Open(filePath)
	if err != nil {
		return make(States,0), fmt.Errorf("openning state file: %s", err)
	}
	defer stateFile.Close()

	var states States
	if err := json.NewDecoder(stateFile).Decode(&states); err != nil {
		return make(States,0), fmt.Errorf("parsing state file: %s", err)
	}

	return states, nil
}

func SaveState(filePath string, state States) error {
	stateFile, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("creating state file: %s", err)
	}
	defer stateFile.Close()

	json.NewEncoder(stateFile).Encode(state)
	return nil
}
