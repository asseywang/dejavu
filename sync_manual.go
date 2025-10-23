// DejaVu - Data snapshot and sync.
// Copyright (c) 2022-present, b3log.org
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package dejavu

import (
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/88250/gulu"
	"github.com/siyuan-note/dejavu/cloud"
	"github.com/siyuan-note/dejavu/entity"
	"github.com/siyuan-note/logging"
)

func (repo *Repo) SyncDownload(context map[string]interface{}) (mergeResult *MergeResult, trafficStat *TrafficStat, err error) {
	lock.Lock()
	defer lock.Unlock()

	// 锁定云端，防止其他设备并发上传数据
	err = repo.tryLockCloud(repo.DeviceID, context)
	if nil != err {
		return
	}
	defer repo.unlockCloud(context)

	mergeResult = &MergeResult{Time: time.Now()}
	trafficStat = &TrafficStat{m: &sync.Mutex{}}

	// 获取本地最新索引
	latest, err := repo.Latest()
	if nil != err {
		logging.LogErrorf("get latest failed: %s", err)
		return
	}

	// 从云端获取最新索引
	length, cloudLatest, err := repo.downloadCloudLatest(context)
	if nil != err {
		if !errors.Is(err, cloud.ErrCloudObjectNotFound) {
			logging.LogErrorf("download cloud latest failed: %s", err)
			return
		}
	}
	trafficStat.DownloadFileCount++
	trafficStat.DownloadBytes += length
	trafficStat.APIGet++

	if cloudLatest.ID == latest.ID || "" == cloudLatest.ID {
		// 数据一致或者云端为空，直接返回
		return
	}

	// 计算本地缺失的文件
	fetchFileIDs, err := repo.localNotFoundFiles(cloudLatest.Files)
	if nil != err {
		logging.LogErrorf("get local not found files failed: %s", err)
		return
	}

	// 从云端下载缺失文件并入库
	length, fetchedFiles, err := repo.downloadCloudFilesPut(fetchFileIDs, context)
	if nil != err {
		logging.LogErrorf("download cloud files put failed: %s", err)
		return
	}
	trafficStat.DownloadFileCount += len(fetchFileIDs)
	trafficStat.DownloadBytes += length
	trafficStat.APIGet += trafficStat.DownloadFileCount

	// 组装还原云端最新文件列表
	cloudLatestFiles, err := repo.getFiles(cloudLatest.Files)
	if nil != err {
		logging.LogErrorf("get cloud latest files failed: %s", err)
		return
	}

	// 从文件列表中得到去重后的分块列表
	cloudChunkIDs := repo.getChunks(cloudLatestFiles)

	// 计算本地缺失的分块
	fetchChunkIDs, err := repo.localNotFoundChunks(cloudChunkIDs)
	if nil != err {
		logging.LogErrorf("get local not found chunks failed: %s", err)
		return
	}

	// 从云端下载缺失分块并入库
	length, err = repo.downloadCloudChunksPut(fetchChunkIDs, context)
	trafficStat.DownloadBytes += length
	trafficStat.DownloadChunkCount += len(fetchChunkIDs)
	trafficStat.APIGet += trafficStat.DownloadChunkCount

	// 计算本地相比上一个同步点的 upsert 和 remove 差异
	latestFiles, err := repo.getFiles(latest.Files)
	if nil != err {
		logging.LogErrorf("get latest files failed: %s", err)
		return
	}
	latestSync := repo.latestSync()
	latestSyncFiles, err := repo.getFiles(latestSync.Files)
	if nil != err {
		logging.LogErrorf("get latest sync files failed: %s", err)
		return
	}
	localUpserts, localRemoves := repo.diffUpsertRemove(latestFiles, latestSyncFiles, false)
	localChanged := 0 < len(localUpserts) || 0 < len(localRemoves)

	// 计算云端最新相比本地最新的 upsert 和 remove 差异
	// 在单向同步的情况下该结果可直接作为合并结果
	mergeResult.Upserts, mergeResult.Removes = repo.diffUpsertRemove(cloudLatestFiles, latestFiles, false)

	var fetchedFileIDs []string
	for _, fetchedFile := range fetchedFiles {
		fetchedFileIDs = append(fetchedFileIDs, fetchedFile.ID)
	}

	// 计算冲突的 upsert
	// 冲突的文件以云端 upsert 和 remove 为准
	for _, localUpsert := range localUpserts {
		if nil != repo.getFile(mergeResult.Upserts, localUpsert) || nil != repo.getFile(mergeResult.Removes, localUpsert) {
			mergeResult.Conflicts = append(mergeResult.Conflicts, localUpsert)
			logging.LogInfof("sync download conflict [%s, %s, %s]", localUpsert.ID, localUpsert.Path, time.UnixMilli(localUpsert.Updated).Format("2006-01-02 15:04:05"))
		}
	}

	// 冲突文件复制到数据历史文件夹
	if 0 < len(mergeResult.Conflicts) {
		now := mergeResult.Time.Format("2006-01-02-150405")
		temp := filepath.Join(repo.TempPath, "repo", "sync", "conflicts", now)
		for i, file := range mergeResult.Conflicts {
			var checkoutTmp *entity.File
			checkoutTmp, err = repo.store.GetFile(file.ID)
			if nil != err {
				logging.LogErrorf("get file failed: %s", err)
				return
			}

			err = repo.checkoutFile(checkoutTmp, temp, i+1, len(mergeResult.Conflicts), context)
			if nil != err {
				logging.LogErrorf("checkout file failed: %s", err)
				return
			}

			absPath := filepath.Join(temp, checkoutTmp.Path)
			err = repo.genSyncHistory(now, file.Path, absPath)
			if nil != err {
				logging.LogErrorf("generate sync history failed: %s", err)
				err = ErrCloudGenerateConflictHistory
				return
			}
		}
	}

	// 数据变更后还原文件
	err = repo.restoreFiles(mergeResult, context)
	if nil != err {
		logging.LogErrorf("restore files failed: %s", err)
	}

	// 处理合并
	err = repo.mergeSync(mergeResult, localChanged, false, latest, cloudLatest, cloudChunkIDs, trafficStat, context)
	if nil != err {
		logging.LogErrorf("merge sync failed: %s", err)
		return
	}

	// 统计流量
	go repo.cloud.AddTraffic(&cloud.Traffic{
		DownloadBytes: trafficStat.DownloadBytes,
		APIGet:        trafficStat.APIGet,
	})

	// 移除空目录
	gulu.File.RemoveEmptyDirs(repo.DataPath, removeEmptyDirExcludes...)
	return
}

func (repo *Repo) SyncUpload(context map[string]interface{}) (trafficStat *TrafficStat, err error) {
	lock.Lock()
	defer lock.Unlock()

	// 锁定云端，防止其他设备并发上传数据
	err = repo.tryLockCloud(repo.DeviceID, context)
	if nil != err {
		return
	}
	defer repo.unlockCloud(context)

	trafficStat = &TrafficStat{m: &sync.Mutex{}}

	latest, err := repo.Latest()
	if nil != err {
		logging.LogErrorf("get latest failed: %s", err)
		return
	}

	// 从云端获取最新索引
	length, cloudLatest, err := repo.downloadCloudLatest(context)
	if nil != err {
		if !errors.Is(err, cloud.ErrCloudObjectNotFound) {
			logging.LogErrorf("download cloud latest failed: %s", err)
			return
		}
	}
	trafficStat.DownloadFileCount++
	trafficStat.DownloadBytes += length
	trafficStat.APIPut++

	if cloudLatest.ID == latest.ID {
		// 数据一致，直接返回
		return
	}

	availableSize := repo.cloud.GetAvailableSize()
	if availableSize <= cloudLatest.Size || availableSize <= latest.Size {
		err = ErrCloudStorageSizeExceeded
		return
	}

	// 计算云端缺失的文件
	var uploadFiles []*entity.File
	for _, localFileID := range latest.Files {
		if !gulu.Str.Contains(localFileID, cloudLatest.Files) {
			var uploadFile *entity.File
			uploadFile, err = repo.store.GetFile(localFileID)
			if nil != err {
				logging.LogErrorf("get file failed: %s", err)
				return
			}
			uploadFiles = append(uploadFiles, uploadFile)
		}
	}

	// 从文件列表中得到去重后的分块列表
	uploadChunkIDs := repo.getChunks(uploadFiles)

	// 这里暂时不计算云端缺失的分块了，因为目前计数云端缺失分块的代价太大
	//uploadChunkIDs, err = repo.cloud.GetChunks(uploadChunkIDs)
	//if nil != err {
	//	logging.LogErrorf("get cloud repo upload chunks failed: %s", err)
	//	return
	//}

	// 上传分块
	length, err = repo.uploadChunks(uploadChunkIDs, context)
	if nil != err {
		logging.LogErrorf("upload chunks failed: %s", err)
		return
	}
	trafficStat.UploadChunkCount += len(uploadChunkIDs)
	trafficStat.UploadBytes += length
	trafficStat.APIPut += trafficStat.UploadChunkCount

	// 上传文件
	length, err = repo.uploadFiles(uploadFiles, context)
	if nil != err {
		logging.LogErrorf("upload files failed: %s", err)
		return
	}
	trafficStat.UploadChunkCount += len(uploadFiles)
	trafficStat.UploadBytes += length
	trafficStat.APIPut += trafficStat.UploadChunkCount

	// 更新云端索引信息
	err = repo.updateCloudIndexes(latest, trafficStat, context)
	if nil != err {
		logging.LogErrorf("update cloud indexes failed: %s", err)
		return
	}

	// 更新本地同步点
	err = repo.UpdateLatestSync(latest)
	if nil != err {
		logging.LogErrorf("update latest sync failed: %s", err)
		return
	}

	// 统计流量
	go repo.cloud.AddTraffic(&cloud.Traffic{
		UploadBytes: trafficStat.UploadBytes,
		APIPut:      trafficStat.APIPut,
	})
	return
}
