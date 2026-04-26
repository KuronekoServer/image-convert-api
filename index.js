const { serve } = require('@hono/node-server');
const { Hono } = require('hono');
const { cors } = require('hono/cors');
const path = require('node:path');
const sharp = require('sharp');
const AdmZip = require('adm-zip');

const app = new Hono();
const port = 12901;

let ApiRateCount = 0;

const getMimeType = (format) => {
    switch (format) {
        case 'jpg':
        case 'jpeg':
            return 'image/jpeg';
        case 'tif':
        case 'tiff':
            return 'image/tiff';
        default:
            return `image/${format}`;
    }
};

const getUploadedFiles = (formData) => {
    return formData
        .getAll('images')
        .filter((value) => value && typeof value.arrayBuffer === 'function');
};

const buildConvertedFileName = (fileName, format) => {
    const parsedPath = path.parse(path.basename(fileName || 'image'));
    const baseName = parsedPath.name || 'image';

    return `${baseName}.${format}`;
};

const buildContentDisposition = (fileName) => {
    const asciiFileName = fileName.replace(/["\\]/g, '_');
    const utf8FileName = encodeURIComponent(fileName);

    return `attachment; filename="${asciiFileName}"; filename*=UTF-8''${utf8FileName}`;
};

const buildUniqueFileName = (fileName, usedNames) => {
    const parsedPath = path.parse(fileName);
    let candidate = fileName;
    let counter = 1;

    while (usedNames.has(candidate)) {
        candidate = `${parsedPath.name} (${counter}).${parsedPath.ext.replace(/^\./, '')}`;
        counter += 1;
    }

    usedNames.add(candidate);
    return candidate;
};

app.use('/api/*', cors());

app.post('/api/convert', async (c) => {
    ApiRateCount++;

    try {
        const formData = await c.req.formData();
        const files = getUploadedFiles(formData);

        if (files.length === 0) {
            return c.text('No images uploaded.', 400);
        }

        const toFormat = (c.req.query('toFormat') || 'webp').toLowerCase();

        if (files.length === 1) {
            const inputBuffer = Buffer.from(await files[0].arrayBuffer());
            const outputBuffer = await sharp(inputBuffer).toFormat(toFormat).toBuffer();
            const outputFileName = buildConvertedFileName(files[0].name, toFormat);

            return c.body(outputBuffer, 200, {
                'Content-Type': getMimeType(toFormat),
                'Content-Disposition': buildContentDisposition(outputFileName),
            });
        }

        const convertedImages = await Promise.all(
            files.map(async (file) => {
                const inputBuffer = Buffer.from(await file.arrayBuffer());
                return sharp(inputBuffer).toFormat(toFormat).toBuffer();
            })
        );

        const zip = new AdmZip();
        const usedNames = new Set();

        convertedImages.forEach((image, index) => {
            const outputFileName = buildConvertedFileName(files[index].name, toFormat);
            const uniqueFileName = buildUniqueFileName(outputFileName, usedNames);

            zip.addFile(uniqueFileName, image);
        });

        return c.body(await zip.toBufferPromise(), 200, {
            'Content-Type': 'application/zip',
            'Content-Disposition': 'attachment; filename="converted_images.zip"',
        });
    } catch (error) {
        console.error(error);
        return c.text('Internal Server Error', 500);
    }
});

app.get('/health', (c) => c.text('OK'));

app.get('/metrics', (c) => c.text(`ApiRateCount ${ApiRateCount}`));

serve({ fetch: app.fetch, port }, (info) => {
    console.log(`Server is running on port ${info.port}`);
});
