const express = require('express');
const sharp = require('sharp');
const multer = require('multer');
const fs = require('fs');
const path = require('path');
const AdmZip = require('adm-zip');

const app = express();
const port = 12901;

let ApiRateCount = 0;

// 保存先のディレクトリを作成
const outputDirectory = path.join(__dirname, 'output');
const zipDirectory = path.join(__dirname, 'zip');
fs.mkdirSync(outputDirectory, { recursive: true });
fs.mkdirSync(zipDirectory, { recursive: true });

// Multerの設定
const storage = multer.memoryStorage();
const upload = multer({ storage: storage });

app.options('/api/convert', (req, res) => {
  res.setHeader('Access-Control-Allow-Origin', '*');
  res.setHeader('Access-Control-Allow-Methods', 'POST, OPTIONS');
  res.setHeader('Access-Control-Allow-Headers', 'Content-Type');
  res.sendStatus(200);
});

// ルートエンドポイント
app.post('/api/convert', upload.array('images'), async (req, res) => {
    ApiRateCount++; // APIへのアクセス数をカウント
    try {
        if (!req.files || req.files.length === 0) {
            return res.status(400).send('No images uploaded.');
        }

        const toFormat = req.query.toFormat || 'webp';

        const convertedImages = [];

        // ファイルが1つの場合
        if (req.files.length === 1) {
            const inputBuffer = req.files[0].buffer;
            const outputBuffer = await sharp(inputBuffer).toFormat(toFormat).toBuffer();
            res.type(toFormat);
            res.send(outputBuffer);
            return;
        } else {
            // ファイルが複数の場合
            await Promise.all(
                req.files.map(async (file, index) => {
                    const inputBuffer = file.buffer;
                    const outputBuffer = await sharp(inputBuffer).toFormat(toFormat).toBuffer();
                    convertedImages.push(outputBuffer);
                })
            );
        }

        if (req.files.length > 1) {
            const zip = new AdmZip();
            convertedImages.forEach((image, index) => {
                zip.addFile(`${index + 1}.${toFormat}`, image);
            });

            const zipFilePath = path.join(zipDirectory, 'converted_images.zip');
            res.send(await zip.toBufferPromise());
        }
    } catch (error) {
        console.error(error);
        res.status(500).send('Internal Server Error');
    }
});

app.get('/health', (req, res) => {
    res.set('Content-Type', 'text/plain');
    res.send('OK');
});

// メトリクスエンドポイント
app.get('/metrics', async (req, res) => {
    let metrics = `ApiRateCount ${ApiRateCount}`;
    res.set('Content-Type', 'text/plain');
    res.send(metrics);
});

app.listen(port, () => {
    console.log(`Server is running on port ${port}`);
});
