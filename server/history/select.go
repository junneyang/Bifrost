package history

import (
	"unsafe"
	"time"
	"strings"
	"strconv"
	"fmt"
	"database/sql/driver"
	pluginDriver "github.com/brokercap/Bifrost/plugin/driver"
	"github.com/brokercap/Bifrost/server"
	"github.com/brokercap/Bifrost/server/count"
	"github.com/brokercap/Bifrost/config"
	"log"
	"runtime/debug"
)

func (This *History) threadStart(i int)  {
	log.Println("history threadStart start:",i,This.DbName,This.SchemaName,This.TableName)
	defer func() {
		log.Println("history threadStart over:",i,This.DbName,This.SchemaName,This.TableName)
		This.threadResultChan <- i
		if err :=recover();err!=nil{
			This.ThreadPool[i].Error = fmt.Errorf( fmt.Sprint(err) + string(debug.Stack()) )
			log.Println("history threadStart:",fmt.Sprint(err) + string(debug.Stack()))
		}
	}()
	This.ThreadPool[i] = &ThreadStatus{
		Num:i+1,
		Error:nil,
		NowStartI:0,
	}
	db := DBConnect(This.Uri)
	db.Exec("SET NAMES UTF8",[]driver.Value{})
	This.initMetaInfo(db)
	if len(This.Fields) == 0{
		This.ThreadPool[i].Error = fmt.Errorf("Fields empty,%s %s %s "+This.DbName,This.SchemaName,This.TableName)
		log.Println("history Fields empty",This.DbName,This.SchemaName,This.TableName)
		return
	}

	var toServerList []*server.ToServer
	toServerList = make([]*server.ToServer,0)

	dbSouceInfo := server.GetDBObj(This.DbName)
	for _,toServerInfo := range dbSouceInfo.GetTable(This.SchemaName,This.TableName).ToServerList{
		for _,ID := range This.ToServerIDList{
			if ID == toServerInfo.ToServerID{
				toServerList = append(toServerList,toServerInfo)
				break
			}
		}
	}
	countChan := dbSouceInfo.GetChannel(dbSouceInfo.GetTable(This.SchemaName,This.TableName).ChannelKey).GetCountChan()
	CountKey := This.SchemaName + "_-" + This.TableName
	var sendToServerResult = func(ToServerInfo *server.ToServer,pluginData *pluginDriver.PluginDataType)  {
		ToServerInfo.Lock()
		status := ToServerInfo.Status
		if status == "deling" || status == "deled"{
			ToServerInfo.Unlock()
			return
		}
		if status == ""{
			ToServerInfo.Status = "running"
		}
		ToServerInfo.QueueMsgCount++
		if ToServerInfo.ToServerChan == nil{
			ToServerInfo.ToServerChan = &server.ToServerChan{
				To:     make(chan *pluginDriver.PluginDataType, config.ToServerQueueSize),
			}
			go ToServerInfo.ConsumeToServer(dbSouceInfo,pluginData.SchemaName,pluginData.TableName)
		}
		ToServerInfo.Unlock()
		ToServerInfo.ToServerChan.To <- pluginData
	}
	n := len(This.Fields)
	var start uint64
	var sql string
	for {
		sql,start = This.GetNextSql()
		if sql == ""{
			break
		}
		stmt, err := db.Prepare(sql)
		if err != nil{
			This.ThreadPool[i].Error = err
			log.Println("history threadStart err:",err,"sql:",sql, This.DbName,This.SchemaName,This.TableName)
			return
		}
		This.ThreadPool[i].NowStartI = start
		p:=make([]driver.Value,0)
		rows, err := stmt.Query(p)
		if err != nil{
			This.ThreadPool[i].Error = err
			return
		}
		rowCount := 0
		for {
			if This.Status == HISTORY_STATUS_KILLED{
				return
			}
			dest := make([]driver.Value, n, n)
			err := rows.Next(dest)
			if err != nil {
				break
			}
			rowCount++
			m := make(map[string]interface{}, n)
			sizeCount := int64(0)
			for i, v := range This.Fields {
				if dest[i] == nil{
					m[*v.COLUMN_NAME] = dest[i]
					continue
				}
				switch *v.DATA_TYPE {
				case "set":
					m[*v.COLUMN_NAME] = strings.Split(dest[i].(string), ",")
					break
				case "tinyint(1)":
					switch fmt.Sprint(dest[i]) {
					case "1":
						m[*v.COLUMN_NAME] = true
						break
					case "0":
						m[*v.COLUMN_NAME] = false
						break
					default:
						m[*v.COLUMN_NAME] = dest[i]
						break
					}
					break
				default:
					m[*v.COLUMN_NAME] = dest[i]
					break
				}
				sizeCount += int64(unsafe.Sizeof(m[*v.COLUMN_NAME]))
			}
			if len(m) == 0{
				return
			}
			Rows := make([]map[string]interface{},1)
			Rows[0] = m
			d := &pluginDriver.PluginDataType{
				Timestamp:		uint32(time.Now().Unix()),
				EventType: 		"insert",
				Rows:           Rows,
				Query:          "",
				SchemaName:		This.SchemaName,
				TableName:		This.TableName,
				BinlogFileNum:	0,
				BinlogPosition:	0,
				Pri:			This.TablePriArr,
			}

			for _,toServerInfo := range toServerList{
				sendToServerResult(toServerInfo,d)
			}

			countChan <- &count.FlowCount{
				//Time:"",
				Count:1,
				TableId:CountKey,
				ByteSize:sizeCount*int64(len(toServerList)),
			}
		}
		rows.Close()
		stmt.Close()

		if This.TablePriKeyMaxId == 0 && rowCount < This.Property.ThreadCountPer {
			return
		}
	}
}

func (This *History) GetNextSql() (sql string,start uint64){
	var where string = ""
	if This.Property.LimitOptimize == 0 || This.TablePriKeyMaxId  == 0 {
		if This.NowStartI > This.TableInfo.TABLE_ROWS{
			return
		}
		if This.Property.Where != "" {
			where = " WHERE " + This.Property.Where
		}
		This.Lock()
		start = This.NowStartI
		This.NowStartI += uint64(This.Property.ThreadCountPer)
		This.Unlock()
		var limit string = ""
		// 假如没有主键 或者 非 InnoDB 引擎，直接 select *from t limit x,y
		limit = " LIMIT " + strconv.FormatUint(start,10) + "," + strconv.Itoa(This.Property.ThreadCountPer)
		if This.TableInfo.ENGINE != "InnoDB" || This.TablePriKey == "" {
			sql = "SELECT * FROM `" + This.SchemaName + "`.`" + This.TableName + "`" + where + limit
		}else{
			// 假如有主键的情况下，采用 join 子查询的方式先分页再 通过 主键去查数据,大分页的情况下，有一定优化作用，innodb下才有效
			// 因为分页实际是找出前面的数据再丢掉，而优先对主键分页，意思只只要优先查出主键来分页就行了，丢掉的数据会大大减少
			sql = "SELECT a.* FROM `" + This.SchemaName + "`.`" + This.TableName + "` AS a "
			sql += " INNER JOIN ("
			sql += " SELECT `"+ This.TablePriKey +"` FROM `" + This.SchemaName + "`.`" + This.TableName + "`"+ where + limit
			sql += " ) AS b"
			sql += " on a."+This.TablePriKey + " = b."+This.TablePriKey
		}
	}else{
		// 假如TablePriKeyMaxId 有最大值，则说明 主键是 数字类型，可以通过 between 来分页
		This.Lock()
		defer This.Unlock()
		//假如最大开始值 已经超过最大Id值了，则说明不需要再去查询了
		if This.NowStartI >= This.TablePriKeyMaxId {
			return
		}
		// BETWEEN NowStartI AND endI
		// BETWEEN 是包含边界的， 等价于  x >= NowStartI AND x <= endI
		var endI uint64
		if This.NowStartI == 0{
			This.NowStartI = This.TablePriKeyMinId
		}
		start = This.NowStartI
		// 这里最大值 - 每次分页数量 是为了 不int内存溢出，避免 NowStartI + ThreadCountPer 大于 uint64
		// 假如 between 右区间 endI 大于 当前 This.NowStartI，则设置 This.NowStartI 为 endI+1，因为 This.NowStartI 是代表下一次查询的开始位置
		if This.TablePriKeyMaxId >= uint64(This.Property.ThreadCountPer) && This.TablePriKeyMaxId - uint64(This.Property.ThreadCountPer) - 1 > This.NowStartI{
			endI = This.NowStartI + uint64(This.Property.ThreadCountPer) - 1
			This.NowStartI = endI + 1
		}else{
			endI = This.TablePriKeyMaxId
			This.NowStartI = endI
		}
		if This.Property.Where == ""{
			where =  " WHERE `"+ This.TablePriKey + "` BETWEEN "+ strconv.FormatUint(start,10)+" AND "+ strconv.FormatUint(endI,10)
		}else{
			where = " WHERE `" + This.TablePriKey + "` BETWEEN "+ strconv.FormatUint(start,10)+" AND "+ strconv.FormatUint(endI,10) + " AND " + This.Property.Where
		}
		sql = "SELECT * FROM `" + This.SchemaName + "`.`" + This.TableName + "` " + where
	}
	return
}