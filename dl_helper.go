package main

import "net/http"

func httpNewRequest(method, url, ua string) (*http.Request, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Referer", "https://pan.quark.cn/")
	return req, nil
}
