# Use the official Node.js image as the base image
FROM node:20-alpine@sha256:1ab6fc5a31d515dc7b6b25f6acfda2001821f2c2400252b6cb61044bd9f9ad48

# Install pnpm
RUN npm install -g pnpm

# Set the working directory to /app
WORKDIR /app

# Copy the package.json and pnpm-lock.yaml files to the working directory
COPY package.json pnpm-lock.yaml ./

# Install dependencies using pnpm
RUN pnpm install 

# Copy the rest of your application code to the working directory
COPY . .

# Build the Next.js application
RUN pnpm build

# Expose port 3000 to the outside world
EXPOSE 3000

# Command to run the Next.js application
CMD ["pnpm", "start"]
