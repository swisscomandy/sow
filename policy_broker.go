package linux_container


import (
	"net/http"
	"strconv"
	"bytes"
)

var Url string

func SetUrl(url string) {
	Url = url
}

func GetUrl() string {
    	
    return "http://" + Url +":8000/spacegroup"
}

func GetPoolID(space string) int {
	response, _ := http.Get(GetUrl()+"?space="+space)
	defer response.Body.Close()
	buf := new(bytes.Buffer)
	buf.ReadFrom(response.Body)
        result, _ := strconv.Atoi(buf.String())
	return result
}
