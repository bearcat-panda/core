package types

type Pod struct {
	Name      string `json:"name"`
	Desc      string `json:"desc"`
	Scheduler string `json:"scheduler"`
}
