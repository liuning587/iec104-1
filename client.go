package iec104

import (
	"context"
	"net"
	"os"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

var (
	retryInterval     = 10 * time.Second
	contextTimeout    = 30 * time.Second
	dialTimeout       = 10 * time.Second
	testInterval      = 20 * time.Second
	totalCallInterval = 15 * time.Minute
)

//Client 104客户端
type Client struct {
	address   string
	conn      *net.TCPConn
	Ctx       context.Context
	cancel    context.CancelFunc
	Logger    *logrus.Logger
	lock      *sync.Mutex
	rsn       int16
	ssn       int16
	DataChan  chan *APDU
	sendChan  chan []byte
	iFrameNum int
	handler   func(c *Client)
}

//NewClient 初始化客户端,连接失败，每隔10秒重试
func NewClient(address string, logger *logrus.Logger) *Client {
	var conn *net.TCPConn
	for {
		addr, err := net.ResolveTCPAddr("tcp4", address)
		if err != nil {
			logger.Fatalln("解析服务器地址失败，请检查配置")
		} else {
			logger.Infoln("尝试连接服务器")
			conn, err = net.DialTCP("tcp4", nil, addr)
			if err != nil {
				logger.Infoln("连接服务器失败，10秒后开始重试")
				time.Sleep(retryInterval)
			} else {
				logger.Infoln("连接服务器成功")
				break
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		address:  address,
		conn:     conn,
		DataChan: make(chan *APDU, 1),
		sendChan: make(chan []byte, 1),
		Ctx:      ctx,
		cancel:   cancel,
		Logger:   logger,
		lock:     new(sync.Mutex),
	}
}

//Start 启动
func (c *Client) Start(f func(c *Client)) {
	c.handler = f
	c.sendUFrame(startDtAct)
	go c.read()
	go c.write()
	go c.handler(c)
	//定时器，每15分钟发送一次总召唤，每20分钟发送一次对时报文
	ticker := time.NewTicker(totalCallInterval)
	for {
		select {
		case <-ticker.C:
			c.Logger.Info("每隔15分钟发送一次总召唤")
			c.sendTotalCall()
		}
	}
}

//Read 读数据
func (c *Client) read() {
	defer c.cancel()
	c.Logger.Info("socket读协程启动")
	for {
		select {
		case <-c.Ctx.Done():
			c.Logger.Info("socket读线程停止")
			c.Close()
		default:
		}
		c.parseData()
	}
}

//Write 写数据
func (c *Client) write() {
	defer c.cancel()
	c.Logger.Info("socket写协程启动")
	for {
		select {
		case <-c.Ctx.Done():
			c.Logger.Info("socket写线程停止")
			c.Close()
		case data := <-c.sendChan:
			_, err := c.conn.Write(data)
			if err != nil {
				c.cancel()
			}
		}

	}
}

//ParseData 解析接收到的数据
func (c *Client) parseData() {
	handleErr := func(tag string, err error) {
		c.Logger.Errorf("%s read socket读操作异常: %v", tag, err)
		if err != nil {
			c.Close()
		}
	}

	buf := make([]byte, 2)
	//读取启动符和长度
	n, err := c.conn.Read(buf)
	if err != nil {
		handleErr("读取启动符和长度", err)
		return
	}
	c.conn.SetDeadline(time.Now().Add(contextTimeout))
	length := int(buf[1])
	//读取正文
	contentBuf := make([]byte, length)
	n, err = c.conn.Read(contentBuf)
	if err != nil {
		handleErr("读取正文", err)
		return
	}
	//长度不够继续读取，直至达到期望长度
	i := 1
	for n < length {
		i++
		nextLength := length - n
		nextBuf := make([]byte, nextLength)
		m, err := c.conn.Read(nextBuf)
		if err != nil {
			handleErr("循环读取正文", err)
			return
		}
		contentBuf = append(contentBuf[:n], nextBuf[:m]...)
		n = len(contentBuf)
		c.Logger.Debugf("循环读取数据，当前为第%d次读取，期望长度:%d,本次长度:%d,当前总长度:%d", i, length, m, n)
	}
	c.Logger.Debugf("收到原始数据: [% X],rsn:%d,ssn:%d,长度:%d", append(buf, contentBuf[:n]...), c.rsn, c.ssn, 2+len(contentBuf[:n]))
	apdu := new(APDU)
	err = apdu.parseAPDU(contentBuf[:n])
	if err != nil {
		c.Logger.Warnf("解析APDU异常: %v", err)
		c.Logger.Panicln("退出程序")
		return
	}
	switch apdu.CtrFrame.(type) {
	case IFrame:
		switch apdu.ASDU.TypeID {
		case CIcNa1:
			if apdu.ASDU.Cause == 7 {
				c.Logger.Info("接收总召唤确认帧")
				c.sendSFrame()
			} else if apdu.ASDU.Cause == 10 {
				c.Logger.Info("接收总召唤结束帧")
				c.sendSFrame()
				c.Logger.Info("发送电度总召唤")
				c.sendElectricityTotalCall()
			}
		case CCiNa1:
			if apdu.ASDU.Cause == 7 {
				c.Logger.Info("接收电度总召唤确认帧")
			} else if apdu.ASDU.Cause == 10 {
				c.Logger.Info("接收电度总召唤结束帧")
			}
			c.sendSFrame()
		default:
			c.iFrameNum++
			c.Logger.Debugf("接收到第%d个I帧", c.iFrameNum)
			c.DataChan <- apdu
			c.sendSFrame()
		}
	case SFrame:
		c.Logger.Debugln("接收到S帧")
		c.DataChan <- apdu
	case UFrame:
		c.Logger.Debugln("接收到U帧")
		uFrame := apdu.CtrFrame.(UFrame)
		switch uFrame.cmd {
		case startDtCon:
			c.Logger.Info("U帧为启动确认帧，发送总召唤")
			c.sendTotalCall()
		case testFrAct:
			c.Logger.Info("U帧为测试激活帧,发送测确认帧")
			c.sendUFrame(testFrCon)
		}
	default:
		c.Logger.Debugln("接收到未知帧")
	}
}

//sendUFrame 发送U帧
func (c *Client) sendUFrame(cmd [4]byte) {
	data := convertBytes(convert4BytesToSlice(cmd))
	c.Logger.Debugf("发送启动U帧: [% X]", data)
	c.sendChan <- data
}

//sendSFrame 发送S帧
func (c *Client) sendSFrame() {
	c.incrRsn()
	rsnBytes := parseLittleEndianUInt16(uint16(c.rsn << 1))
	sendBytes := make([]byte, 0, 0)
	sendBytes = append(sendBytes, 0x01, 0x00)
	sendBytes = append(sendBytes, rsnBytes...)
	data := convertBytes(sendBytes)
	c.Logger.Debugf("发送启动S帧: [% X]", data)
	c.sendChan <- data
}

//sendTotalCall 发送总召唤
func (c *Client) sendTotalCall() {
	ssnBytes := parseLittleEndianUInt16(uint16(c.ssn << 1))
	rsnBytes := parseLittleEndianUInt16(uint16(c.rsn << 1))
	totalCallData := make([]byte, 0, 0)
	totalCallData = append(totalCallData, ssnBytes...)
	totalCallData = append(totalCallData, rsnBytes...)
	totalCallData = append(totalCallData, 0x64, 0x01, 0x06, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x14)
	data := convertBytes(totalCallData)
	c.Logger.Debugf("发送总召唤: [% X]", data)
	c.sendChan <- data
}

//sendTotalCall 发送电度总召唤
func (c *Client) sendElectricityTotalCall() {
	ssnBytes := parseLittleEndianUInt16(uint16(c.ssn << 1))
	rsnBytes := parseLittleEndianUInt16(uint16(c.rsn << 1))
	totalCallData := make([]byte, 0, 0)
	totalCallData = append(totalCallData, ssnBytes...)
	totalCallData = append(totalCallData, rsnBytes...)
	totalCallData = append(totalCallData, 0x65, 0x01, 0x06, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x05)
	data := convertBytes(totalCallData)
	c.Logger.Debugf("发送电度总召唤: [% X]", data)
	c.sendChan <- data
}

//incrRsn 增加rsn
func (c *Client) incrRsn() {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.rsn++
	if c.rsn < 0 {
		c.rsn = 0
	}
}

//Close 结束程序
func (c *Client) Close() {
	c.cancel()
	c.conn.Close()
	c.Logger.Println("断开服务器连接，程序关闭")
	os.Exit(1)
}
