package api

type locationDTO struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type showDTO struct {
	ID    int64  `json:"id"`
	Start string `json:"start"`
	End   string `json:"end"`
}

type concertDTO struct {
	ID       int64       `json:"id"`
	Artist   string      `json:"artist"`
	Location locationDTO `json:"location"`
	Shows    []showDTO   `json:"shows"`
}

type seatingRowDTO struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Seats struct {
		Total       int   `json:"total"`
		Unavailable []int `json:"unavailable"`
	} `json:"seats"`
}

